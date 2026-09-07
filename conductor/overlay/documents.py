#!/usr/bin/env python3
"""Explicit Markdown outbox consumed by the existing conductor Telegram Bot.

The offline CLI never loads bridge configuration or credentials. Authorization
files are a local operator contract, not isolation from other same-user processes.
"""
from __future__ import annotations

import argparse
import asyncio
import hashlib
import json
import math
import os
import re
import sqlite3
import stat
import sys
import time
import uuid
from pathlib import Path

SCHEMA = "slavna.document-send.v1"
MAX_BYTES = 1_048_576
MAX_PENDING = 8
POLL_SECONDS = 2


class DocumentError(RuntimeError):
    """Input or state cannot safely authorize document delivery."""


def positive_int(value):
    return type(value) is int and value > 0


def validate_document(filename, content, caption):
    if not isinstance(filename, str) or not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_. -]{0,120}\.md", filename):
        raise DocumentError("document filename must be a safe .md basename")
    if not isinstance(content, bytes) or not 0 < len(content) <= MAX_BYTES:
        raise DocumentError("document must contain 1..1048576 bytes")
    try:
        decoded = content.decode("utf-8")
        size = len(caption.encode("utf-16-le")) // 2 if isinstance(caption, str) else 0
    except UnicodeError:
        raise DocumentError("document and caption must be valid Unicode") from None
    if "\x00" in decoded or not isinstance(caption, str) or not caption.strip() or not 1 <= size <= 1024:
        raise DocumentError("document or caption is invalid")
    if any(ord(char) < 32 and char not in "\n\t" for char in caption):
        raise DocumentError("caption contains control characters")


def no_symlinks(path):
    if not path.is_absolute() or ".." in path.parts or "\x00" in str(path):
        raise DocumentError("path must be absolute without traversal")
    for candidate in (*reversed(path.parents), path):
        if candidate.is_symlink():
            raise DocumentError("symlinks are not allowed")


def read_input(path, limit):
    no_symlinks(path)
    if any(part.startswith(".env") or part in {".secrets", "auth.json", "env"} for part in path.parts):
        raise DocumentError("credential paths cannot be document inputs")
    try:
        descriptor = os.open(str(path), os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
        with os.fdopen(descriptor, "rb") as stream:
            info = os.fstat(stream.fileno())
            if (not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid()
                    or info.st_mode & 0o022 or info.st_nlink != 1 or not 0 < info.st_size <= limit):
                raise DocumentError("input must be bounded, regular and owner-controlled")
            raw = stream.read(limit + 1)
        if len(raw) > limit:
            raise DocumentError("input exceeded its bound")
        return raw
    except OSError:
        raise DocumentError("document input is unavailable") from None


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise DocumentError("duplicate authorization field")
        result[key] = value
    return result


def validate_id(value):
    try:
        if not isinstance(value, str) or str(uuid.UUID(value)) != value:
            raise ValueError()
    except (ValueError, AttributeError):
        raise DocumentError("authorization_id must be a canonical UUID") from None


class Outbox:
    def __init__(self, path):
        no_symlinks(path)
        path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
        info = path.parent.stat()
        if info.st_uid != os.getuid() or info.st_mode & 0o077:
            raise DocumentError("outbox directory must be private and owned")
        for candidate in (path, Path(str(path) + "-wal"), Path(str(path) + "-shm")):
            no_symlinks(candidate)
            if candidate.exists():
                info = candidate.stat()
                if (not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid()
                        or info.st_mode & 0o077 or info.st_nlink != 1):
                    raise DocumentError("outbox files must be private, regular and owned")
        # Create privately before SQLite opens it; sidecars inherit its mode.
        try:
            fd = os.open(str(path), os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
        except FileExistsError:
            pass
        else:
            os.close(fd)
        self.connection = sqlite3.connect(str(path), timeout=5, isolation_level=None)
        self.connection.row_factory = sqlite3.Row
        try:
            self.connection.execute("PRAGMA journal_mode=WAL")
            self.connection.execute("PRAGMA synchronous=FULL")
            self.connection.execute("""CREATE TABLE IF NOT EXISTS documents (
                authorization_id TEXT PRIMARY KEY, sha256 TEXT NOT NULL,
                authorization_json TEXT NOT NULL, filename TEXT NOT NULL,
                caption TEXT NOT NULL, content BLOB NOT NULL,
                expires_at INTEGER NOT NULL, created_at REAL NOT NULL,
                state TEXT NOT NULL CHECK(state IN
                    ('pending','unknown','api_accepted','rejected','expired')),
                chat_id INTEGER, message_id INTEGER, detail TEXT NOT NULL
            )""")
        except BaseException:
            self.connection.close()
            raise

    def close(self):
        self.connection.close()

    def enqueue(self, ready_path, *, now):
        if ready_path.name != "SEND-READY.json":
            raise DocumentError("authorization must be named SEND-READY.json")
        try:
            ready = json.loads(read_input(ready_path, 16_384), object_pairs_hook=unique_object)
        except (UnicodeError, json.JSONDecodeError):
            raise DocumentError("invalid authorization JSON") from None
        fields = {"schema", "authorization_id", "authorized", "recipient", "artifact_path",
                  "sha256", "caption", "expires_at"}
        if (not isinstance(ready, dict) or set(ready) != fields
                or ready.get("schema") != SCHEMA or ready.get("authorized") is not True
                or ready.get("recipient") != "configured_operator"
                or not isinstance(ready.get("artifact_path"), str)
                or not isinstance(ready.get("sha256"), str)
                or not re.fullmatch(r"[0-9a-f]{64}", ready["sha256"])
                or not positive_int(ready.get("expires_at")) or not math.isfinite(now)):
            raise DocumentError("document authorization is invalid")
        aid = ready["authorization_id"]
        validate_id(aid)
        authorization = json.dumps(ready, sort_keys=True, separators=(",", ":"))
        # An exact replay returns the original receipt, even after source removal
        # or expiry. It cannot authorize a new send or replace stored bytes.
        previous = self.connection.execute(
            "SELECT authorization_json FROM documents WHERE authorization_id = ?", (aid,)
        ).fetchone()
        if previous is not None:
            if previous["authorization_json"] != authorization:
                raise DocumentError("authorization_id already belongs to another request")
            return self.receipt(aid)
        if not now < ready["expires_at"] <= now + 86_400:
            raise DocumentError("document authorization is expired or too far in the future")
        source = Path(ready["artifact_path"])
        validate_document(source.name, b"placeholder", ready["caption"])
        content = read_input(source, MAX_BYTES)
        validate_document(source.name, content, ready["caption"])
        digest = hashlib.sha256(content).hexdigest()
        if digest != ready["sha256"]:
            raise DocumentError("document SHA-256 does not match authorization")
        self.connection.execute("BEGIN IMMEDIATE")
        try:
            previous = self.connection.execute(
                "SELECT authorization_json FROM documents WHERE authorization_id = ?", (aid,)
            ).fetchone()
            if previous is not None:
                if previous["authorization_json"] != authorization:
                    raise DocumentError("authorization_id already belongs to another request")
            else:
                if self.connection.execute(
                    "SELECT 1 FROM documents WHERE sha256 = ? AND state IN ('pending','unknown')", (digest,)
                ).fetchone():
                    raise DocumentError("this document has a pending or unknown delivery; inspect it first")
                if self.connection.execute("SELECT COUNT(*) FROM documents WHERE state = 'pending'").fetchone()[0] >= MAX_PENDING:
                    raise DocumentError("document outbox is full")
                self.connection.execute("""INSERT INTO documents
                    (authorization_id, sha256, authorization_json, filename, caption, content,
                     expires_at, created_at, state, detail)
                    VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'pending', 'awaiting_bridge')""",
                    (aid, digest, authorization, source.name, ready["caption"], content, ready["expires_at"], now))
            self.connection.execute("COMMIT")
        except BaseException:
            self.connection.execute("ROLLBACK")
            raise
        return self.receipt(aid)

    def receipt(self, aid):
        validate_id(aid)
        row = self.connection.execute("""SELECT authorization_id, sha256, filename, state,
            chat_id, message_id, detail FROM documents WHERE authorization_id = ?""", (aid,)).fetchone()
        if row is None:
            raise DocumentError("document receipt does not exist")
        return {"schema": SCHEMA, **dict(row), "desktop_observed": False}

    async def dispatch_one(self, bot, user_id, *, now):
        if not positive_int(user_id) or not math.isfinite(now):
            raise DocumentError("invalid configured document recipient or timestamp")
        self.connection.execute("""UPDATE documents SET state = 'expired', detail = 'authorization_expired'
            WHERE state = 'pending' AND expires_at <= ?""", (now,))
        row = self.connection.execute(
            "SELECT * FROM documents WHERE state = 'pending' ORDER BY created_at, authorization_id LIMIT 1"
        ).fetchone()
        if row is None:
            return None
        aid = row["authorization_id"]
        # FULL-synchronous autocommit BEFORE the first await / external effect.
        # Cancellation, process death or receipt-write failure can never requeue it.
        claimed = self.connection.execute("""UPDATE documents SET state = 'unknown', chat_id = ?,
            detail = 'dispatch_started_result_unconfirmed'
            WHERE authorization_id = ? AND state = 'pending'""", (user_id, aid)).rowcount
        if claimed != 1:
            return None
        try:
            validate_document(row["filename"], row["content"], row["caption"])
            if hashlib.sha256(row["content"]).hexdigest() != row["sha256"]:
                raise DocumentError("snapshot integrity mismatch")
        except DocumentError:
            self._finish(aid, "rejected", "snapshot_invalid")
            return self.receipt(aid)
        try:
            message_id = await send_document(bot, user_id, row["filename"], row["content"], row["caption"])
        except Rejected:
            self._finish(aid, "rejected", "telegram_rejected")
        except Exception:
            # Never persist/log exception strings: SDK errors may include secrets.
            return self.receipt(aid)
        else:
            self._finish(aid, "api_accepted", "telegram_api_success_desktop_unverified", message_id)
        return self.receipt(aid)

    def _finish(self, aid, state, detail, message_id=None):
        self.connection.execute("UPDATE documents SET state = ?, detail = ?, message_id = ? WHERE authorization_id = ?",
                                (state, detail, message_id, aid))


class Rejected(DocumentError):
    """Telegram explicitly rejected this attempt; never retry automatically."""


async def send_document(bot, user_id, filename, content, caption):
    # Lazy import: offline submission/status do not need aiogram or a Bot token.
    from aiogram.types import BufferedInputFile
    from aiogram.exceptions import (TelegramBadRequest, TelegramForbiddenError,
                                    TelegramNotFound, TelegramRetryAfter)
    validate_document(filename, content, caption)
    if not positive_int(user_id):
        raise DocumentError("invalid document recipient")
    try:
        message = await bot.send_document(
            chat_id=user_id, document=BufferedInputFile(content, filename=filename),
            caption=caption, parse_mode=None, request_timeout=45,
        )
    except (TelegramBadRequest, TelegramForbiddenError, TelegramNotFound, TelegramRetryAfter):
        raise Rejected("Telegram rejected the document") from None
    except Exception:
        raise DocumentError("Telegram document result is unknown") from None
    document = getattr(message, "document", None)
    chat = getattr(message, "chat", None)
    if (not positive_int(getattr(message, "message_id", None))
            or not positive_int(getattr(chat, "id", None)) or chat.id != user_id
            or getattr(chat, "type", None) != "private"
            or not isinstance(getattr(document, "file_id", None), str) or not document.file_id
            or getattr(document, "file_name", None) != filename
            or type(getattr(document, "file_size", None)) is not int or document.file_size != len(content)):
        raise DocumentError("Telegram document success could not be verified")
    return message.message_id


def register(dp, *, database, user_id, log):
    """Attach one local outbox task to the existing dispatcher's lifecycle."""
    if not positive_int(user_id):
        raise DocumentError("document outbox needs the configured operator id")
    task = None
    outbox = None

    async def consume(bot):
        while True:
            try:
                await outbox.dispatch_one(bot, user_id, now=time.time())
            except Exception:
                log.error("overlay: document outbox failed closed; inspect local receipt")
            await asyncio.sleep(POLL_SECONDS)

    async def start(bot):
        nonlocal task, outbox
        if task is not None and not task.done():
            return
        try:
            outbox = Outbox(database)
        except Exception:
            log.error("overlay: document outbox startup failed closed; document delivery disabled")
            return
        task = asyncio.create_task(consume(bot))
        log.info("overlay: document outbox ready")

    async def stop():
        nonlocal task, outbox
        if task is not None:
            task.cancel()
            try:
                await task
            except asyncio.CancelledError:
                pass
            task = None
        if outbox is not None:
            outbox.close()
            outbox = None

    dp.startup.register(start)
    dp.shutdown.register(stop)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--database", type=Path, required=True)
    mode = parser.add_mutually_exclusive_group(required=True)
    mode.add_argument("--enqueue", type=Path, metavar="SEND_READY_JSON")
    mode.add_argument("--status", metavar="AUTHORIZATION_ID")
    args = parser.parse_args(argv)
    outbox = None
    try:
        if not args.database.is_file():
            raise DocumentError("CLI requires an outbox initialized by the deployed bridge")
        outbox = Outbox(args.database)
        receipt = outbox.enqueue(args.enqueue, now=time.time()) if args.enqueue else outbox.receipt(args.status)
        print(json.dumps(receipt, sort_keys=True))
        return 0 if receipt["state"] in {"pending", "api_accepted"} else 2
    except (DocumentError, OSError, sqlite3.Error) as error:
        # Only our bounded validation messages are safe to expose.
        reason = str(error) if isinstance(error, DocumentError) else "outbox storage unavailable"
        print("Slavna document refused: " + reason, file=sys.stderr)
        return 1
    finally:
        if outbox is not None:
            outbox.close()


if __name__ == "__main__":
    raise SystemExit(main())
