"""Opt-in Slavna adapter to the reviewed shared ledger; owns no token or engine."""
from __future__ import annotations

import asyncio
import fcntl
import functools
import hashlib
import json
import math
import os
from pathlib import Path
import re
import stat
import subprocess
import sys
import tempfile

from documents import DocumentError, positive_int, read_input

OUTBOX_PROTOCOL = 2
MAX_FILE = 20 * 1024 * 1024
MAX_DOWNLOAD_DISK = 256 * 1024 * 1024
RESTRICTED_NAME = re.compile(r"(?i)^(?:\.env(?:\..*)?|\.secrets?|auth(?:\..*)?|credentials?(?:\..*)?|id_(?:rsa|ed25519)|.*\.(?:pem|key|p12|pfx))$")


class Refused(RuntimeError):
    """Refused before invoking the shared CLI or Telegram send."""


class Unknown(RuntimeError):
    """A started command may have committed; never replay automatically."""


def owned_json(path):
    return json.loads(read_input(path, 65536))


def private_directory(path):
    if not path.is_absolute() or path.resolve() != path:
        raise Refused('noncanonical private directory')
    path.mkdir(parents=True, mode=0o700, exist_ok=True)
    info = path.stat()
    if info.st_uid != os.getuid() or info.st_mode & 0o077:
        raise Refused('directory is not private and owned')


class Client:
    def __init__(self, home=None, command=subprocess.run):
        self.home = Path(home) if home is not None else Path.home()
        self.root = self.home/'.local/share/telegram-agents'
        self.state = self.home/'.local/state/telegram-agents'
        self.marker = self.root/'shared-slavna-ready.json'
        self.bindings = self.home/'.config/telegram-agents/bindings.json'
        self.command = command
        self.identity = None

    def opted_in(self):
        # A malformed/dangling marker still owns ingress and must fail closed.
        return self.marker.exists() or self.marker.is_symlink()

    def validate(self):
        try:
            receipt, marker = owned_json(self.root/'installation.json'), owned_json(self.marker)
            row = owned_json(self.bindings)['bots']['slavna']
            head, thread = marker['head'], marker['thread']
            if (set(marker) != {'head','thread'} or not isinstance(head, str) or not re.fullmatch(r'[0-9a-f]{40}', head)
                    or not isinstance(thread, str) or not re.fullmatch(r'[0-9a-f-]{36}', thread)
                    or receipt.get('head') != head or row.get('thread') != thread
                    or row.get('enabled') is not True or row.get('external') is not True):
                raise Refused('shared Slavna activation differs')
            identity = (head, thread)
            if self.identity is not None and self.identity != identity:
                raise Refused('shared generation changed; reviewed bridge restart required')
            directory = self.root/head
            manifest = owned_json(directory/'manifest.json')
            cli = directory/'cli.py'
            if (manifest.get('head') != head
                    or hashlib.sha256(read_input(cli, 1024*1024)).hexdigest() != manifest['files']['cli.py']):
                raise Refused('shared CLI bundle differs')
            self.identity = identity
            return cli
        except (OSError, KeyError, TypeError, ValueError, DocumentError):
            raise Refused('shared Slavna activation unavailable') from None

    def call(self, operation, *args, payload=None):
        cli = self.validate()
        # Bridge credentials are not inherited by the credential-free CLI.
        environment = {key:value for key,value in os.environ.items()
                       if key in ('PATH', 'HOME', 'LANG', 'LC_ALL', 'TMPDIR')}
        argv = [sys.executable, '-E', '-s', str(cli), '--state', str(self.state),
                operation, '--bot', 'slavna', *map(str, args)]
        raw = json.dumps(payload).encode() if payload is not None else b''
        if len(raw) > 262144: raise Refused('ingress frame exceeds bound')
        try:
            result = self.command(argv, input=raw,
                                  capture_output=True, timeout=30, cwd=str(cli.parent), env=environment)
        except FileNotFoundError:
            raise Refused('shared CLI could not start') from None
        except (OSError, subprocess.SubprocessError):
            raise Unknown('shared CLI result unknown') from None
        if result.returncode or len(result.stdout) > 262144:
            raise Unknown('shared CLI result unavailable')
        try: return json.loads(result.stdout)
        except (ValueError, TypeError): raise Unknown('shared CLI response invalid') from None

    async def acall(self, operation, *args, payload=None):
        loop = asyncio.get_running_loop()
        return await loop.run_in_executor(None, functools.partial(self.call, operation, *args, payload=payload))


class BoundedWriter:
    """aiogram's existing download helper writes into this bounded private stream."""
    def __init__(self, stream, maximum):
        self.stream, self.maximum, self.written = stream, maximum, 0

    def write(self, data):
        if self.written + len(data) > self.maximum:
            raise Refused('attachment exceeds declared or absolute bound')
        count = self.stream.write(data)
        self.written += count
        return count

    def seek(self, *args): return self.stream.seek(*args)
    def tell(self): return self.stream.tell()
    def flush(self): return self.stream.flush()


def supported(message):
    text = getattr(message, 'text', None)
    if isinstance(text, str) and text.lstrip().startswith('/'):
        return False
    return bool(text or any(getattr(message, kind, None) for kind in ('voice','audio','photo','document')))


def frame_for(message, user_id):
    """Allowlisted message fields, not a dump of Telegram's surrounding context."""
    sender, chat = getattr(message, 'from_user', None), getattr(message, 'chat', None)
    if (not positive_int(user_id) or getattr(sender, 'id', None) != user_id
            or getattr(sender, 'is_bot', None) is not False
            or getattr(chat, 'id', None) != user_id or getattr(chat, 'type', None) != 'private'):
        raise Refused('private operator message required')
    mid = getattr(message, 'message_id', None)
    date = getattr(message, 'date', None)
    if hasattr(date, 'timestamp'): date = int(date.timestamp())
    if not positive_int(mid) or type(date) is not int:
        raise Refused('stable message identity missing')
    body = {'message_id':mid, 'date':date, 'from':{'id':user_id,'is_bot':False},
            'chat':{'id':user_id,'type':'private'}}
    for key in ('text','caption'):
        value = getattr(message, key, None)
        if value is not None:
            if not isinstance(value, str) or len(value.encode()) > 65536:
                raise Refused('message text exceeds bounds')
            body[key] = value
    forward_keys = ('forward_origin','forward_from','forward_from_chat','forward_sender_name',
                    'forward_date','is_automatic_forward','via_bot','sender_chat')
    if any(getattr(message, key, None) for key in forward_keys):
        # Never copy third-party identity/chat objects; retain explicit provenance.
        body['forward_origin'] = {'type':'external', 'source':'Telegram forwarding metadata'}
    obj, kind = None, None
    for field in ('voice','audio','document'):
        if getattr(message, field, None): obj, kind = getattr(message, field), field; break
    photos = getattr(message, 'photo', None)
    if obj is None and photos: obj, kind = photos[-1], 'document'
    if obj is not None:
        original = getattr(obj, 'file_name', None)
        if original is not None and (not isinstance(original, str) or len(original) > 255
                or '/' in original or '\\' in original or RESTRICTED_NAME.fullmatch(original)):
            raise Refused('original attachment name is restricted')
        size, fid = getattr(obj, 'file_size', None), getattr(obj, 'file_id', None)
        if not positive_int(size) or size > MAX_FILE or not isinstance(fid, str) or not 0 < len(fid) <= 1024:
            raise Refused('attachment metadata exceeds bounds')
        meta = {'file_size':size, 'file_id':fid}
        mime = 'image/jpeg' if photos else getattr(obj, 'mime_type', None)
        if mime is not None and (not isinstance(mime, str) or len(mime) > 200):
            raise Refused('invalid raw MIME type')
        if isinstance(mime, str): mime = mime.split(';',1)[0].strip().lower()
        if kind == 'document' and mime in (None, '', 'application/octet-stream') and original:
            mime = {'.ogg':'audio/ogg','.opus':'audio/opus','.mp3':'audio/mpeg',
                    '.m4a':'audio/mp4','.wav':'audio/wav','.flac':'audio/flac',
                    '.webm':'audio/webm'}.get(Path(original).suffix.lower(), mime)
        if mime is not None:
            if not isinstance(mime, str) or len(mime) > 200: raise Refused('invalid MIME type')
            meta['mime_type'] = mime
        duration = getattr(obj, 'duration', None)
        if duration is not None:
            if type(duration) not in (int,float) or not 0 < duration <= 480:
                raise Refused('audio duration exceeds bounds')
            meta['duration'] = duration
        suffix = { 'audio/ogg':'.ogg', 'audio/opus':'.opus', 'audio/mpeg':'.mp3',
                   'audio/mp4':'.m4a', 'audio/x-m4a':'.m4a', 'audio/wav':'.wav',
                   'audio/flac':'.flac', 'image/jpeg':'.jpg', 'image/png':'.png',
                   'application/pdf':'.pdf', 'text/plain':'.txt' }.get(mime, '.bin')
        original_suffix = Path(original).suffix.lower() if original else ''
        if original_suffix in ('.md','.txt','.json','.pdf','.png','.jpg','.jpeg','.gif','.csv',
                               '.wav','.mp3','.m4a','.ogg','.opus','.flac','.mp4','.webm'):
            suffix = original_suffix
        if kind == 'voice': suffix = '.ogg'
        meta['file_name'] = 'telegram-' + str(mid) + suffix
        body[kind] = meta
    return {'update':{'update_id':mid, 'message':body}}, obj


async def notice(client, message, code, log):
    # Notices use the same durable dispatcher/pacing as every public response.
    source = 'ingress-notice:' + str(message.chat.id) + ':' + str(message.message_id) + ':' + code
    try:
        result = await client.acall('notice', '--source', source, '--code', code)
        if not isinstance(result, dict) or result.get('queued') is not True:
            raise Unknown('notice persistence unconfirmed')
    except asyncio.CancelledError: raise
    except Exception:
        log.error('overlay: ingress/notice unavailable; polling stopped before next offset; inspect receipts')
        # aiogram swallows ordinary Exceptions and would ACK this update anyway.
        # Cancellation propagates through sequential polling and runs shutdown.
        raise asyncio.CancelledError('shared ingress unavailable; update unacknowledged') from None


async def ingest(client, message, user_id, log):
    # Caller serializes this entire operation, before its first await.
    client.validate()
    frame, obj = frame_for(message, user_id)
    if obj is None:
        result = await client.acall('ingress', payload=frame)
    else:
        directory = client.state/'slavna-downloads'
        private_directory(directory)
        used = sum(p.stat().st_size for p in directory.glob('input-*/telegram-input.bin')
                   if not p.is_symlink())
        if used + obj.file_size > MAX_DOWNLOAD_DISK:
            raise Refused('download disk budget exhausted; inspect orphaned inputs')
        # Temporary content is kept until the CLI has made its immutable snapshot.
        with tempfile.TemporaryDirectory(prefix='input-', dir=directory) as temporary:
            path = Path(temporary)/'telegram-input.bin'
            fd = os.open(path, os.O_CREAT | os.O_EXCL | os.O_WRONLY | os.O_NOFOLLOW, 0o600)
            with os.fdopen(fd, 'wb') as stream:
                output = BoundedWriter(stream, obj.file_size)
                try:
                    await asyncio.wait_for(message.bot.download(obj, destination=output, timeout=45), timeout=50)
                except asyncio.CancelledError: raise
                except Exception: raise Refused('attachment download did not complete') from None
                stream.flush()
                os.fsync(stream.fileno())
                if output.written != obj.file_size: raise Refused('download size differs')
            frame['file'] = {'path':str(path), 'name':'telegram-input.bin'}
            result = await client.acall('ingress', payload=frame)
    if not isinstance(result, dict) or result.get('accepted') is not True:
        await notice(client, message, 'rejected', log)
    # Progress, canonical prompt echo and answer all come through the durable outbox.
    return result


def telegram_error(exc):
    try:
        from aiogram.exceptions import (TelegramRetryAfter, TelegramBadRequest,
                                        TelegramForbiddenError, TelegramNotFound)
    except ImportError: return ('unknown', None)
    if isinstance(exc, TelegramRetryAfter):
        delay = exc.retry_after
        if type(delay) in (int,float) and math.isfinite(delay) and delay >= 0:
            return ('retry', delay)
    if isinstance(exc, (TelegramBadRequest, TelegramForbiddenError, TelegramNotFound)):
        return ('failed', None)
    return ('unknown', None)


def document_payload(body, state):
    if not isinstance(body, dict): raise Refused('invalid document claim')
    path = Path(body.get('path', ''))
    directory = state/'snapshots'
    if path.parent != directory:
        raise Refused('document is not an immutable shared snapshot')
    name, size, digest = body.get('name'), body.get('size'), body.get('sha256')
    if (not isinstance(name, str) or not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9._-]{0,119}', name)
            or RESTRICTED_NAME.fullmatch(name)
            or not isinstance(digest, str) or not re.fullmatch(r'[0-9a-f]{64}', digest)
            or not positive_int(size) or size > MAX_FILE
            or path.name != digest + Path(name).suffix.lower()):
        raise Refused('invalid document snapshot metadata')
    data = read_input(path, MAX_FILE)
    if len(data) != size or hashlib.sha256(data).hexdigest() != digest:
        raise Refused('document snapshot changed')
    caption = body.get('caption', '')
    if not isinstance(caption, str) or len(caption.encode('utf-16-le'))//2 > 1024:
        raise Refused('invalid document caption')
    return data, name, caption


async def send_claim(bot, row, user_id, state):
    if (not isinstance(row, dict) or not positive_int(row.get('id'))
            or row.get('chat') != user_id or not positive_int(user_id)):
        raise Refused('invalid output claim recipient')
    document = None
    if row.get('kind') == 'text':
        text = row.get('body')
        if not isinstance(text, str) or not 0 < len(text.encode('utf-16-le'))//2 <= 4096:
            raise Refused('invalid text claim')
        result = await bot.send_message(chat_id=user_id, text=text, parse_mode=None, request_timeout=45)
    elif row.get('kind') == 'document':
        from aiogram.types import BufferedInputFile
        data, name, caption = document_payload(row.get('body'), state)
        document = (name, len(data))
        result = await bot.send_document(chat_id=user_id, document=BufferedInputFile(data, filename=name),
                                         caption=caption, parse_mode=None, request_timeout=45)
    else: raise Refused('unsupported output claim')
    chat = getattr(result, 'chat', None)
    if (not positive_int(getattr(result, 'message_id', None)) or getattr(chat, 'id', None) != user_id
            or getattr(chat, 'type', None) != 'private'):
        raise Unknown('Telegram acceptance could not be verified')
    if document:
        item = getattr(result, 'document', None)
        if (getattr(item, 'file_name', None), getattr(item, 'file_size', None)) != document:
            raise Unknown('Telegram document receipt differs')
    return result.message_id


async def pump_once(client, bot, user_id, log):
    # An incompatible CLI must refuse before creating a claim or Bot send.
    row = await client.acall('outbox-claim', '--protocol', OUTBOX_PROTOCOL)
    if row is None: return
    # Claim is already durable unknown BEFORE the first network await.
    identity = row.get('id') if isinstance(row, dict) else None
    if not positive_int(identity): raise Unknown('shared claim identity unavailable')
    attempt = row.get('attempt')
    if not positive_int(attempt): raise Unknown('shared claim generation unavailable')
    args = []
    try:
        client.validate()  # Refuse a cutover between claim and send.
        mid = await send_claim(bot, row, user_id, client.state)
    except asyncio.CancelledError:
        # Durable unknown survives shutdown; no unsafe final retry.
        raise
    except (Refused, DocumentError): outcome = 'failed'
    except Exception as exc:
        outcome, delay = telegram_error(exc)
        if delay is not None: args = ['--retry-after', delay]
    else:
        outcome, args = 'sent', ['--message-id', mid]
    try:
        result = await client.acall('outbox-result', '--id', identity, '--attempt', attempt,
                                    '--state', outcome, *args)
    except Exception:
        log.error('overlay: shared output receipt unavailable; delivery may have happened; no replay')
        raise
    if isinstance(result, dict) and result.get('applied') is False:
        # The owner may have resolved the row while HTTP was in flight. The
        # central ledger owns terminal state; never replay or overwrite it.
        log.info('overlay: shared output result superseded by owner or newer claim; no replay')
        return
    if outcome == 'unknown': log.warning('overlay: shared output unknown; inspect receipt; no replay')


def register(dp, *, user_id, is_authorized, log, home=None):
    client = Client(home)
    if not client.opted_in(): return False
    if not positive_int(user_id): raise Refused('configured private operator required')
    admission = asyncio.Lock()
    task, owner = None, None

    async def on_shared_message(message):
        # Unauthorized/group content is swallowed here instead of entering legacy handlers.
        sender, chat = getattr(message, 'from_user', None), getattr(message, 'chat', None)
        if (getattr(sender, 'id', None) != user_id or getattr(sender, 'is_bot', None) is not False
                or getattr(chat, 'id', None) != user_id or getattr(chat, 'type', None) != 'private'
                or not is_authorized(message)):
            return
        async with admission:
            try: await ingest(client, message, user_id, log)
            except asyncio.CancelledError: raise
            except (Refused, DocumentError): await notice(client, message, 'rejected', log)
            except Exception:
                log.error('overlay: shared ingress unknown; no legacy fallback or replay')
                await notice(client, message, 'unavailable', log)

    async def consume(bot):
        while True:
            try: await pump_once(client, bot, user_id, log)
            except asyncio.CancelledError: raise
            except Exception: log.error('overlay: shared pump unavailable; inspect durable receipts')
            await asyncio.sleep(1)

    async def start(bot):
        nonlocal task, owner
        if task is not None and not task.done(): return
        try:
            client.validate()
            private_directory(client.state)
            owner = os.open(client.state/'slavna-outbox.lock', os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW, 0o600)
            info = os.fstat(owner)
            if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o077:
                raise Refused('unsafe shared output lock')
            fcntl.flock(owner, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except Exception:
            if owner is not None: os.close(owner); owner = None
            log.error('overlay: shared startup refused; ingress stays guarded, inspect receipt')
            return
        task = asyncio.create_task(consume(bot))
        log.info('overlay: shared Slavna ready; prior unknown claims are never replayed')

    async def stop():
        nonlocal task, owner
        if task is not None:
            task.cancel()
            try: await task
            except asyncio.CancelledError: pass
            task = None
        if owner is not None: os.close(owner); owner = None

    original_polling = dp.start_polling
    @functools.wraps(original_polling)
    async def sequential_polling(*args, **kwargs):
        # aiogram advances getUpdates offset only after this update finishes.
        kwargs['handle_as_tasks'] = False
        return await original_polling(*args, **kwargs)

    dp.start_polling = sequential_polling
    dp.message.register(on_shared_message, supported)
    dp.startup.register(start)
    dp.shutdown.register(stop)
    log.info('overlay: shared Slavna owns non-command ingress; serial durable admission enabled')
    return True
