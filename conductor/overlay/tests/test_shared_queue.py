"""Credential-free SDK/CLI fixtures for shared Slavna admission and output."""
import asyncio
from datetime import datetime, timezone
import hashlib
import importlib.util
import json
import logging
import os
from pathlib import Path
import subprocess
import sys
import tempfile
from types import SimpleNamespace as S
import unittest
from unittest.mock import AsyncMock, Mock, patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import shared_queue as sq

HEAD, NEXT, THREAD = 'a'*40, 'b'*40, 'c'*8+'-'+ 'c'*4+'-'+ 'c'*4+'-'+ 'c'*4+'-'+ 'c'*12


def write_json(path, data):
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    path.write_text(json.dumps(data))
    path.chmod(0o600)


def setup_home(home):
    client = sq.Client(home)
    write_json(client.root/'installation.json', {'head':HEAD})
    write_json(client.marker, {'head':HEAD, 'thread':THREAD})
    write_json(client.bindings, {'bots':{'slavna':{'enabled':True,'external':True,'thread':THREAD}}})
    code = b'# reviewed synthetic CLI\n'
    path = client.root/HEAD/'cli.py'
    path.parent.mkdir(mode=0o700)
    path.write_bytes(code)
    path.chmod(0o500)
    write_json(path.parent/'manifest.json', {'head':HEAD,'files':{'cli.py':hashlib.sha256(code).hexdigest()}})
    return client


def message(**updates):
    data = dict(text='hola', from_user=S(id=42, is_bot=False), chat=S(id=42,type='private'),
                message_id=11, date=datetime(2026,9,7,tzinfo=timezone.utc),
                answer=AsyncMock(), bot=S(download=AsyncMock()))
    data.update(updates)
    return S(**data)


def receipt(mid=55):
    return S(message_id=mid, chat=S(id=42,type='private'))


class Base(unittest.IsolatedAsyncioTestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.home = Path(self.tmp.name).resolve()
        self.client = setup_home(self.home)
        self.log = logging.getLogger('shared-test')


class ClientTests(Base):
    def test_generation_thread_and_manifest_are_revalidated(self):
        self.assertEqual(self.client.validate().name, 'cli.py')
        write_json(self.client.marker, {'head':NEXT,'thread':THREAD})
        with self.assertRaises(sq.Refused): self.client.validate()
        write_json(self.client.marker, {'head':HEAD,'thread':THREAD})
        write_json(self.client.bindings, {'bots':{'slavna':{'enabled':True,'external':False,'thread':THREAD}}})
        with self.assertRaises(sq.Refused): self.client.validate()

    def test_dangling_symlink_opts_in_but_fails_closed(self):
        self.client.marker.unlink()
        self.client.marker.symlink_to(self.home/'missing')
        self.assertTrue(self.client.opted_in())
        with self.assertRaises(sq.Refused): self.client.validate()

    def test_cli_never_inherits_tokens_or_python_import_overrides(self):
        self.client.command = Mock(return_value=S(returncode=0,stdout=b'null'))
        with patch.dict(os.environ, {'FAKE_TOKEN':'fixture','PYTHONPATH':'/wrong'}):
            self.assertIsNone(self.client.call('outbox-claim'))
        args, kwargs = self.client.command.call_args
        self.assertEqual(args[0][1:3], ['-E','-s'])
        self.assertNotIn('FAKE_TOKEN', kwargs['env'])
        self.assertNotIn('PYTHONPATH', kwargs['env'])
        self.assertNotIn('.env', ' '.join(args[0]))

    def test_cli_timeout_and_malformed_reply_are_unknown_without_retry(self):
        for outcome in (subprocess.TimeoutExpired('fixture',30), S(returncode=0,stdout=b'incomplete')):
            self.client.command = Mock(side_effect=outcome) if isinstance(outcome, Exception) else Mock(return_value=outcome)
            with self.assertRaises(sq.Unknown): self.client.call('ingress',payload={})
            self.assertEqual(self.client.command.call_count,1)

    def test_file_integrity_refuses_cli_before_start(self):
        cli = self.client.root/HEAD/'cli.py'
        cli.chmod(0o600)
        cli.write_bytes(b'changed')
        self.client.command = Mock()
        with self.assertRaises(sq.Refused): self.client.call('outbox-claim')
        self.client.command.assert_not_called()


class IngressTests(Base):
    def test_minimal_frame_private_auth_date_and_forwarding(self):
        frame, obj = sq.frame_for(message(via_bot=S(id=7)),42)
        body = frame['update']['message']
        self.assertIsNone(obj)
        self.assertIs(type(body['date']),int)
        self.assertEqual(frame['update']['update_id'],body['message_id'])
        self.assertIn('forward_origin',body)
        self.assertNotIn('via_bot',body)
        for delta in ({'chat':S(id=-100,type='group')},{'from_user':S(id=9,is_bot=False)},
                      {'from_user':S(id=42,is_bot=True)}):
            with self.assertRaises(sq.Refused): sq.frame_for(message(**delta),42)

    def test_commands_do_not_enter_shared_ingress(self):
        self.assertFalse(sq.supported(message(text='/status')))
        self.assertFalse(sq.supported(message(text=' /peek slavna')))
        self.assertTrue(sq.supported(message()))
        self.assertTrue(sq.supported(message(text=None,voice=S(file_size=3))))

    async def test_bounded_download_is_snapshotted_before_temporary_cleanup(self):
        obj = S(file_size=3,file_id='synthetic-file',duration=2,mime_type='audio/ogg')
        msg = message(text=None,voice=obj)
        async def download(obj, destination, **kwargs): destination.write(b'abc')
        msg.bot.download.side_effect = download
        paths = []
        async def call(operation, payload):
            path = Path(payload['file']['path'])
            self.assertEqual(path.read_bytes(),b'abc')
            self.assertEqual(path.stat().st_mode & 0o777,0o600)
            paths.append(path)
            return {'accepted':True,'job':1,'new':True}
        self.client.acall = AsyncMock(side_effect=call)
        await sq.ingest(self.client,msg,42,self.log)
        self.assertFalse(paths[0].exists())
        msg.answer.assert_not_called()  # No Heard/transcript/progress duplicate.

    async def test_oversized_or_mismatched_download_never_enqueues(self):
        obj = S(file_size=3,file_id='synthetic-file',duration=2)
        for content in (b'1234',b'12'):
            msg = message(text=None,voice=obj)
            async def download(obj, destination, **kwargs): destination.write(content)
            msg.bot.download.side_effect = download
            self.client.acall = AsyncMock()
            with self.assertRaises(sq.Refused): await sq.ingest(self.client,msg,42,self.log)
            self.client.acall.assert_not_called()


    async def test_original_secret_or_traversal_name_refused_before_download(self):
        for name in ('.env','.env.production','auth.json','credentials.json','id_ed25519','secret.pem','../report.md'):
            msg=message(text=None,document=S(file_id='fixture',file_size=3,file_name=name,mime_type='text/plain'))
            self.client.acall=AsyncMock()
            with self.subTest(name=name),self.assertRaises(sq.Refused):
                await sq.ingest(self.client,msg,42,self.log)
            msg.bot.download.assert_not_called()
            self.client.acall.assert_not_called()

    def test_safe_original_suffix_survives_generated_basename(self):
        frame,_=sq.frame_for(message(text=None,document=S(file_id='fixture',file_size=3,
            file_name='Informe final.MD',mime_type='text/markdown')),42)
        self.assertEqual(frame['update']['message']['document']['file_name'],'telegram-11.md')


    def test_audio_document_without_specific_mime_preserves_audio_classification(self):
        for mime in (None, '', 'application/octet-stream'):
            frame,_=sq.frame_for(message(text=None,document=S(file_id='fixture',file_size=3,
                file_name='voice.m4a',mime_type=mime)),42)
            meta=frame['update']['message']['document']
            self.assertEqual(meta['mime_type'],'audio/mp4')
            self.assertEqual(meta['file_name'],'telegram-11.m4a')
        frame,_=sq.frame_for(message(text=None,document=S(file_id='fixture',file_size=3,
            file_name='voice.m4a',mime_type='video/mp4')),42)
        self.assertEqual(frame['update']['message']['document']['mime_type'],'video/mp4')


    def test_unknown_suffix_preserves_generic_mime(self):
        frame,_=sq.frame_for(message(text=None,document=S(file_id='fixture',file_size=3,
            file_name='attachment.unknown',mime_type='application/octet-stream')),42)
        meta=frame['update']['message']['document']
        self.assertEqual(meta['mime_type'],'application/octet-stream')
        self.assertEqual(meta['file_name'],'telegram-11.bin')

    def test_uppercase_generic_mime_uses_audio_suffix(self):
        frame,_=sq.frame_for(message(text=None,document=S(file_id='fixture',file_size=3,
            file_name='voice.m4a',mime_type='APPLICATION/OCTET-STREAM')),42)
        self.assertEqual(frame['update']['message']['document']['mime_type'],'audio/mp4')

    def test_parameterized_mime_is_trimmed_and_normalized(self):
        frame,_=sq.frame_for(message(text=None,document=S(file_id='fixture',file_size=3,
            file_name='voice.m4a',mime_type='  Audio/MP4 ; codecs="mp4a.40.2"  ')),42)
        self.assertEqual(frame['update']['message']['document']['mime_type'],'audio/mp4')

    async def test_raw_mime_limit_applies_before_normalization_and_download(self):
        for mime in ('audio/mp4; '+'x'*500,' '*201+'audio/mp4','x'*201):
            msg=message(text=None,document=S(file_id='fixture',file_size=3,
                file_name='voice.m4a',mime_type=mime))
            self.client.acall=AsyncMock()
            with self.subTest(mime_length=len(mime)),self.assertRaises(sq.Refused):
                await sq.ingest(self.client,msg,42,self.log)
            msg.bot.download.assert_not_called()
            self.client.acall.assert_not_called()
        prefix='audio/mp4;'
        exactly_200=prefix+'x'*(200-len(prefix))
        frame,_=sq.frame_for(message(text=None,document=S(file_id='fixture',file_size=3,
            file_name='voice.m4a',mime_type=exactly_200)),42)
        self.assertEqual(frame['update']['message']['document']['mime_type'],'audio/mp4')


class PumpTests(Base):
    def row(self): return {'id':1,'chat':42,'kind':'text','body':'public response','attempt':7}

    async def test_claim_precedes_send_and_receipt_has_proven_message_id(self):
        events = []
        async def call(operation,*args,**kw):
            events.append(operation)
            return self.row() if operation=='outbox-claim' else {}
        async def send(**kwargs):
            events.append('send')
            self.assertIsNone(kwargs['parse_mode'])
            return receipt()
        self.client.acall = AsyncMock(side_effect=call)
        await sq.pump_once(self.client,S(send_message=send),42,self.log)
        self.assertEqual(events,['outbox-claim','send','outbox-result'])
        self.assertIn('sent',self.client.acall.call_args.args)
        self.assertIn(55,self.client.acall.call_args.args)
        self.assertEqual(self.client.acall.call_args_list[0].args,('outbox-claim','--protocol',2))
        args=self.client.acall.call_args.args
        self.assertEqual(args[args.index('--attempt')+1],7)

    async def test_timeout_and_bad_receipt_unknown_never_resend(self):
        for result in (TimeoutError('fixture'),receipt(mid=0)):
            self.client.acall = AsyncMock(side_effect=[self.row(),{}])
            send = AsyncMock(side_effect=result) if isinstance(result,Exception) else AsyncMock(return_value=result)
            await sq.pump_once(self.client,S(send_message=send),42,self.log)
            self.assertEqual(send.await_count,1)
            self.assertIn('unknown',self.client.acall.call_args.args)
            args=self.client.acall.call_args.args
            self.assertEqual(args[args.index('--attempt')+1],7)

    async def test_explicit_429_passes_durable_retry_deadline_only(self):
        self.client.acall = AsyncMock(side_effect=[self.row(),{}])
        with patch.object(sq,'telegram_error',return_value=('retry',37)):
            await sq.pump_once(self.client,S(send_message=AsyncMock(side_effect=RuntimeError('429 fixture'))),42,self.log)
        args = self.client.acall.call_args.args
        self.assertIn('retry',args)
        self.assertEqual(args[-2:],('--retry-after',37))
        self.assertEqual(args[args.index('--attempt')+1],7)

    async def test_wrong_recipient_and_stale_activation_are_never_sent(self):
        row = self.row(); row['chat']=99
        self.client.acall = AsyncMock(side_effect=[row,{}])
        bot = S(send_message=AsyncMock())
        await sq.pump_once(self.client,bot,42,self.log)
        bot.send_message.assert_not_called()
        self.assertIn('failed',self.client.acall.call_args.args)
        args=self.client.acall.call_args.args
        self.assertEqual(args[args.index('--attempt')+1],7)

    async def test_missing_or_invalid_claim_generation_never_sends(self):
        for attempt in (None,0,-1,True,'7',1.5):
            row=self.row();row['attempt']=attempt
            self.client.acall=AsyncMock(return_value=row)
            bot=S(send_message=AsyncMock())
            with self.subTest(attempt=attempt),self.assertRaises(sq.Unknown):
                await sq.pump_once(self.client,bot,42,self.log)
            bot.send_message.assert_not_called()
            self.assertEqual(self.client.acall.await_count,1)

    async def test_incompatible_protocol_stops_before_send_or_result(self):
        self.client.command=Mock(return_value=S(returncode=2,stdout=b'{}'))
        bot=S(send_message=AsyncMock())
        with self.assertRaises(sq.Unknown):
            await sq.pump_once(self.client,bot,42,self.log)
        bot.send_message.assert_not_called()
        self.assertEqual(self.client.command.call_count,1)
        argv=self.client.command.call_args.args[0]
        self.assertEqual(argv[argv.index('--protocol')+1],'2')

    async def test_owner_resolution_during_http_never_replays_or_overwrites(self):
        for outcome in ('sent','unknown','retry'):
            owner={'state':'unknown'}
            async def call(operation,*args,**kwargs):
                if operation=='outbox-claim': return self.row()
                self.assertEqual(owner['state'],'resolved')
                self.assertEqual(args[args.index('--attempt')+1],7)
                self.assertEqual(args[args.index('--state')+1],outcome)
                return {'id':1,'state':'resolved','applied':False}
            async def send(**kwargs):
                owner['state']='resolved'
                if outcome!='sent': raise TimeoutError('synthetic HTTP completion')
                return receipt()
            self.client.acall=AsyncMock(side_effect=call)
            bot=S(send_message=AsyncMock(side_effect=send))
            classification=('retry',10) if outcome=='retry' else ('unknown',None)
            with self.subTest(outcome=outcome),patch.object(sq,'telegram_error',return_value=classification):
                await sq.pump_once(self.client,bot,42,self.log)
            self.assertEqual(owner['state'],'resolved')
            self.assertEqual(bot.send_message.await_count,1)
            self.assertEqual(self.client.acall.await_count,2)

    async def test_receipt_write_loss_never_repeats_network_attempt(self):
        self.client.acall = AsyncMock(side_effect=[self.row(),sq.Unknown('lost receipt')])
        bot = S(send_message=AsyncMock(return_value=receipt()))
        with self.assertRaises(sq.Unknown): await sq.pump_once(self.client,bot,42,self.log)
        self.assertEqual(bot.send_message.await_count,1)

    async def test_cancellation_leaves_claim_unknown(self):
        self.client.acall = AsyncMock(return_value=self.row())
        bot = S(send_message=AsyncMock(side_effect=asyncio.CancelledError()))
        with self.assertRaises(asyncio.CancelledError): await sq.pump_once(self.client,bot,42,self.log)
        self.assertEqual(self.client.acall.await_count,1)

    def test_document_snapshot_changed_or_outside_root_is_refused(self):
        content=b'approved file'
        digest=hashlib.sha256(content).hexdigest()
        path=self.client.state/'snapshots'/(digest+'.md')
        path.parent.mkdir(parents=True,mode=0o700)
        path.write_bytes(content)
        body={'path':str(path),'sha256':digest,'name':'report.md','size':len(content)}
        self.assertEqual(sq.document_payload(body,self.client.state)[0],content)
        path.write_bytes(b'changed')
        with self.assertRaises(sq.Refused): sq.document_payload(body,self.client.state)
        body['path']=str(self.home/digest)
        with self.assertRaises(sq.Refused): sq.document_payload(body,self.client.state)


    def test_snapshot_suffix_must_match_authorized_filename(self):
        digest=hashlib.sha256(b'fixture').hexdigest()
        body={'path':str(self.client.state/'snapshots'/(digest+'.txt')),
              'sha256':digest,'name':'report.md','size':7}
        with self.assertRaises(sq.Refused): sq.document_payload(body,self.client.state)


class RegistrationTests(Base):
    def dp(self):
        return S(message=S(register=Mock()),startup=S(register=Mock()),shutdown=S(register=Mock()),
                 start_polling=AsyncMock())

    async def test_opt_in_serializes_polling_and_admission_before_ack(self):
        dp=self.dp(); original=dp.start_polling
        fake=sq.Client(self.home); fake.validate=Mock(return_value=Path('/fixture/cli.py'))
        started, release = asyncio.Event(), asyncio.Event()
        order=[]
        async def ingest(client,msg,user,log):
            order.append(msg.message_id)
            if msg.message_id==1:
                started.set()
                await release.wait()
        with patch.object(sq,'Client',return_value=fake),patch.object(sq,'ingest',side_effect=ingest):
            self.assertTrue(sq.register(dp,user_id=42,is_authorized=lambda _:True,log=self.log))
            await dp.start_polling('existing bot',handle_as_tasks=True)
            self.assertIs(original.call_args.kwargs['handle_as_tasks'],False)
            handler=dp.message.register.call_args.args[0]
            audio=asyncio.create_task(handler(message(message_id=1,text=None,voice=S())))
            await started.wait()
            text=asyncio.create_task(handler(message(message_id=2)))
            await asyncio.sleep(0)
            self.assertEqual(order,[1])
            release.set()
            await asyncio.gather(audio,text)
            self.assertEqual(order,[1,2])

    async def test_disabled_marker_preserves_polling_and_registers_nothing(self):
        self.client.marker.unlink()
        dp=self.dp(); original=dp.start_polling
        self.assertFalse(sq.register(dp,user_id=42,is_authorized=lambda _:True,log=self.log,home=self.home))
        self.assertIs(dp.start_polling,original)
        dp.message.register.assert_not_called()

    async def test_unauthorized_and_group_messages_are_consumed_without_download(self):
        dp=self.dp()
        with patch.object(sq,'ingest',new_callable=AsyncMock) as ingest:
            sq.register(dp,user_id=42,is_authorized=lambda _:True,log=self.log,home=self.home)
            handler=dp.message.register.call_args.args[0]
            await handler(message(chat=S(id=-100,type='group')))
            await handler(message(from_user=S(id=99,is_bot=False)))
            ingest.assert_not_called()

    async def test_ingress_unknown_notice_uses_shared_cli_without_bot_fallback(self):
        dp=self.dp(); msg=message()
        with patch.object(sq,'ingest',side_effect=sq.Unknown('fixture')), patch.object(sq.Client,'acall',new_callable=AsyncMock,return_value={'queued':True}) as call:
            sq.register(dp,user_id=42,is_authorized=lambda _:True,log=self.log,home=self.home)
            await dp.message.register.call_args.args[0](msg)
        self.assertEqual(call.call_args.args,('notice','--source','ingress-notice:42:11:unavailable','--code','unavailable'))
        msg.answer.assert_not_called()

    async def test_active_guard_failure_stops_polling_before_ack_without_fallback(self):
        dp=self.dp(); msg=message()
        sq.register(dp,user_id=42,is_authorized=lambda _:True,log=self.log,home=self.home)
        self.client.marker.unlink()
        with self.assertRaises(asyncio.CancelledError):
            await dp.message.register.call_args.args[0](msg)
        msg.answer.assert_not_called()


    async def test_singleton_pump_lock_released_on_shutdown(self):
        first, second = self.dp(), self.dp()
        for dp in (first,second):
            sq.register(dp,user_id=42,is_authorized=lambda _:True,log=self.log,home=self.home)
        blocker=asyncio.Event()
        async def blocked(*args): await blocker.wait()
        create=asyncio.create_task
        with patch.object(sq,'pump_once',side_effect=blocked), patch('asyncio.create_task',wraps=create) as tasks:
            await first.startup.register.call_args.args[0](S())
            await second.startup.register.call_args.args[0](S())
            self.assertEqual(tasks.call_count,1)
            await first.shutdown.register.call_args.args[0]()
            await second.startup.register.call_args.args[0](S())
            self.assertEqual(tasks.call_count,2)
            await second.shutdown.register.call_args.args[0]()


@unittest.skipUnless(importlib.util.find_spec('aiogram'), 'optional installed SDK; no network')
class RealSDKTests(Base):
    async def test_existing_bot_download_uses_bounded_stream_without_network(self):
        from aiogram import Bot
        from aiogram.types import File
        bot=Bot('123456:'+'x'*32)
        msg=message(text=None,voice=S(file_id='fixture',file_size=3,duration=1),bot=bot)
        async def stream(**kwargs):
            yield b'ab'
            yield b'c'
        async def queued(operation, payload):
            self.assertEqual(Path(payload['file']['path']).read_bytes(),b'abc')
            return {'accepted':True,'job':1,'new':True}
        self.client.acall=AsyncMock(side_effect=queued)
        try:
            with patch.object(bot,'get_file',new=AsyncMock(return_value=File(file_id='fixture',file_unique_id='fixture',file_path='fixture.ogg'))), patch.object(bot.session,'stream_content',new=stream):
                await sq.ingest(self.client,msg,42,self.log)
        finally: await bot.session.close()

    async def test_document_bytes_and_real_sdk429_classification(self):
        from aiogram import Bot
        from aiogram.types import Message
        from aiogram.methods import SendDocument,SendMessage
        from aiogram.exceptions import TelegramRetryAfter
        content=b'approved content'
        digest=hashlib.sha256(content).hexdigest()
        path=self.client.state/'snapshots'/(digest+'.md')
        path.parent.mkdir(parents=True,mode=0o700)
        path.write_bytes(content)
        row={'id':1,'chat':42,'kind':'document','body':{
            'path':str(path),'sha256':digest,'size':len(content),'name':'report.md','caption':'Reporte'}}
        bot=Bot('123456:'+'x'*32)
        async def request(actual_bot,method,timeout=None):
            self.assertIs(actual_bot,bot)
            self.assertIsInstance(method,SendDocument)
            self.assertIsNone(method.parse_mode)
            self.assertEqual(timeout,45)
            self.assertEqual(b''.join([chunk async for chunk in method.document.read(bot)]),content)
            return Message.model_validate({'message_id':9,'date':0,'chat':{'id':42,'type':'private'},
                'document':{'file_id':'fixture','file_unique_id':'fixture','file_name':'report.md','file_size':len(content)}})
        try:
            with patch.object(bot.session,'make_request',side_effect=request):
                self.assertEqual(await sq.send_claim(bot,row,42,self.client.state),9)
            error=TelegramRetryAfter(method=SendMessage(chat_id=42,text='fixture'),message='fixture',retry_after=13)
            self.assertEqual(sq.telegram_error(error),('retry',13))
        finally: await bot.session.close()


    async def test_real_sdk_cancellation_keeps_update_offset_unacknowledged(self):
        from aiogram import Bot,Dispatcher
        from aiogram.types import Update,User
        from aiogram.methods import GetUpdates
        bot=Bot('123456:'+'x'*32)
        dp=Dispatcher()
        sq.register(dp,user_id=42,is_authorized=lambda _:True,log=self.log,home=self.home)
        self.client.marker.unlink()
        update=Update.model_validate({'update_id':100,'message':{'message_id':11,'date':0,
            'chat':{'id':42,'type':'private'},'from':{'id':42,'is_bot':False,'first_name':'Fixture'},'text':'fixture'}})
        requests=[]
        async def request(actual_bot,method,timeout=None):
            self.assertIsInstance(method,GetUpdates)
            requests.append(method)
            if len(requests)>1: self.fail('update acknowledged before durable ingress')
            return [update]
        try:
            with patch.object(bot,'me',new=AsyncMock(return_value=User(id=123456,is_bot=True,first_name='Fixture'))), patch.object(bot.session,'make_request',side_effect=request):
                with self.assertRaises(asyncio.CancelledError):
                    await dp._polling(bot,handle_as_tasks=False)
            self.assertEqual(len(requests),1)
            self.assertIsNone(requests[0].offset)
        finally: await bot.session.close()

    async def test_real_sdk_advances_offset_only_after_durable_admission(self):
        from aiogram import Bot,Dispatcher
        from aiogram.types import Update,User
        from aiogram.methods import GetUpdates
        bot=Bot('123456:'+'x'*32)
        dp=Dispatcher()
        sq.register(dp,user_id=42,is_authorized=lambda _:True,log=self.log,home=self.home)
        update=Update.model_validate({'update_id':100,'message':{'message_id':11,'date':0,
            'chat':{'id':42,'type':'private'},'from':{'id':42,'is_bot':False,'first_name':'Fixture'},'text':'fixture'}})
        events=[]
        async def request(actual_bot,method,timeout=None):
            self.assertIsInstance(method,GetUpdates)
            events.append('get:'+str(method.offset))
            if method.offset is not None: raise asyncio.CancelledError()
            return [update]
        async def admitted(*args):
            events.append('admit')
            await asyncio.sleep(0)
            events.append('committed')
        try:
            with patch.object(bot,'me',new=AsyncMock(return_value=User(id=123456,is_bot=True,first_name='Fixture'))), patch.object(bot.session,'make_request',side_effect=request), patch.object(sq,'ingest',side_effect=admitted):
                with self.assertRaises(asyncio.CancelledError):
                    await dp._polling(bot,handle_as_tasks=False)
            self.assertEqual(events,['get:None','admit','committed','get:101'])
        finally: await bot.session.close()


if __name__=='__main__': unittest.main()
