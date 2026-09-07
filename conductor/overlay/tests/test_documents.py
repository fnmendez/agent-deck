"""Authorization, durable claims and the existing Bot's document transport."""
from __future__ import annotations

import asyncio
import hashlib
import json
import logging
import os
import sqlite3
import sys
import tempfile
import time
import types
import unittest
import uuid
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import documents as d


class OutboxTests(unittest.IsolatedAsyncioTestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.root = Path(self.tmp.name).resolve()
        self.database = self.root / "private" / "outbox.sqlite3"
        self.outbox = d.Outbox(self.database)
        self.content = "# Slavna TEST\n\nPrueba, no es el informe final.\n".encode()
        self.source = self.root / "TEST.md"
        self.source.write_bytes(self.content)
        self.now = time.time()
        self.aid = str(uuid.uuid4())
        self.digest = hashlib.sha256(self.content).hexdigest()
        self.ready = {"schema": d.SCHEMA, "authorization_id": self.aid, "authorized": True,
                      "recipient": "configured_operator", "artifact_path": str(self.source),
                      "sha256": self.digest, "caption": "Prueba de Markdown — Slavna",
                      "expires_at": int(self.now) + 3600}
        self.marker = self.root / "SEND-READY.json"
        self.write_ready()
        self.bot = object()
        self.sender = mock.AsyncMock(return_value=123)
        self.patch = mock.patch.object(d, "send_document", self.sender)
        self.patch.start()

    def tearDown(self):
        self.patch.stop()
        self.outbox.close()
        self.tmp.cleanup()

    def write_ready(self):
        self.marker.write_text(json.dumps(self.ready))

    def enqueue(self):
        return self.outbox.enqueue(self.marker, now=self.now)

    async def dispatch(self, now=None):
        return await self.outbox.dispatch_one(self.bot, 42, now=self.now if now is None else now)

    async def test_snapshot_and_truthful_receipt(self):
        self.assertEqual(self.enqueue()["state"], "pending")
        self.source.write_text("# later draft\n")
        receipt = await self.dispatch()
        self.sender.assert_awaited_once_with(self.bot, 42, "TEST.md", self.content, self.ready["caption"])
        self.assertEqual(receipt["state"], "api_accepted")
        self.assertEqual(receipt["sha256"], self.digest)
        self.assertEqual(receipt["message_id"], 123)
        self.assertEqual(receipt["chat_id"], 42)
        self.assertFalse(receipt["desktop_observed"])
        self.assertNotIn("content", receipt)

    async def test_same_authorization_is_idempotent_even_after_expiry_or_source_removal(self):
        self.enqueue()
        receipt = await self.dispatch()
        self.source.unlink()
        self.assertEqual(self.outbox.enqueue(self.marker, now=self.now + 7200), receipt)
        self.assertIsNone(await self.dispatch())
        self.sender.assert_awaited_once()

    async def test_id_reuse_with_changed_fields_is_refused(self):
        self.enqueue()
        self.ready["caption"] = "another request"
        self.write_ready()
        with self.assertRaisesRegex(d.DocumentError, "another request"):
            self.enqueue()

    async def test_new_explicit_authorization_can_resend_known_result(self):
        for outcome in (d.Rejected("rejected"), 123):
            self.sender.side_effect = outcome if isinstance(outcome, Exception) else None
            self.enqueue()
            await self.dispatch()
            self.ready["authorization_id"] = str(uuid.uuid4())
            self.write_ready()
        self.sender.side_effect = None
        self.assertEqual(self.enqueue()["state"], "pending")
        self.assertEqual((await self.dispatch())["state"], "api_accepted")
        self.assertEqual(self.sender.await_count, 3)

    async def test_new_authorization_cannot_bypass_pending_or_unknown(self):
        self.enqueue()
        self.ready["authorization_id"] = str(uuid.uuid4())
        self.write_ready()
        with self.assertRaisesRegex(d.DocumentError, "pending or unknown"):
            self.enqueue()
        self.sender.side_effect = TimeoutError("secret URL")
        receipt = await self.dispatch()
        self.assertEqual(receipt["state"], "unknown")
        self.assertNotIn("secret", json.dumps(receipt))
        with self.assertRaisesRegex(d.DocumentError, "pending or unknown"):
            self.enqueue()
        self.assertIsNone(await self.dispatch(now=self.now + 5))
        self.sender.assert_awaited_once()

    async def test_invalid_authorization_never_dispatches(self):
        original = dict(self.ready)
        for key, value in (("schema", "other"), ("authorized", False), ("authorized", 1),
                           ("recipient", "arbitrary_chat"), ("authorization_id", "bad"),
                           ("sha256", "0" * 64), ("expires_at", True),
                           ("expires_at", int(self.now) - 1), ("expires_at", int(self.now) + 86402),
                           ("artifact_path", "relative.md"), ("caption", ""),
                           ("caption", "😀" * 513), ("caption", "bad\rheader")):
            self.ready = dict(original, **{key: value})
            self.write_ready()
            with self.subTest(key=key, value=value), self.assertRaises(d.DocumentError):
                self.enqueue()
        self.marker.write_text('{"authorized":false,"authorized":true}')
        with self.assertRaisesRegex(d.DocumentError, "duplicate"):
            self.enqueue()
        self.marker.write_text(json.dumps({**original, "chat_id": 42}))
        with self.assertRaises(d.DocumentError):
            self.enqueue()
        self.marker.unlink()
        with self.assertRaises(d.DocumentError):
            self.enqueue()
        self.assertIsNone(await self.dispatch())
        self.sender.assert_not_awaited()

    async def test_unsafe_source_paths_and_permissions_are_refused(self):
        alias = self.root / "alias.md"
        alias.symlink_to(self.source)
        self.ready["artifact_path"] = str(alias)
        self.write_ready()
        with self.assertRaises(d.DocumentError):
            self.enqueue()
        alias.unlink()
        os.link(str(self.source), str(alias))
        with self.assertRaises(d.DocumentError):
            self.enqueue()
        alias.unlink()
        self.ready["artifact_path"] = str(self.source)
        self.write_ready()
        for path in (self.source, self.marker):
            path.chmod(0o666)
            with self.assertRaises(d.DocumentError):
                self.enqueue()
            path.chmod(0o600)
        for path in (self.root / ".secrets" / "report.md", self.root / ".env", self.root / 'header".md'):
            self.ready["artifact_path"] = str(path)
            self.write_ready()
            with self.assertRaises(d.DocumentError):
                self.enqueue()
        self.sender.assert_not_awaited()

    async def test_bounded_regular_utf8_files(self):
        for content in (b"", b"x" * (d.MAX_BYTES + 1), b"\xff", b"nul\x00"):
            self.source.write_bytes(content)
            self.ready["sha256"] = hashlib.sha256(content).hexdigest()
            self.write_ready()
            with self.assertRaises(d.DocumentError):
                self.enqueue()
        self.source.unlink()
        self.source.mkdir()
        with self.assertRaises(d.DocumentError):
            self.enqueue()

    async def test_expiry_and_destination_gate(self):
        self.enqueue()
        for destination in (0, -42, True, "42"):
            with self.assertRaises(d.DocumentError):
                await self.outbox.dispatch_one(self.bot, destination, now=self.now)
        self.assertIsNone(await self.dispatch(now=self.now + 3601))
        self.assertEqual(self.outbox.receipt(self.aid)["state"], "expired")
        self.sender.assert_not_awaited()

    async def test_independent_connection_cannot_claim_inflight_work(self):
        self.enqueue()
        other = d.Outbox(self.database)
        async def send(*args):
            self.assertEqual(other.receipt(self.aid)["state"], "unknown")
            self.assertIsNone(await other.dispatch_one(self.bot, 42, now=self.now))
            return 123
        self.sender.side_effect = send
        try:
            self.assertEqual((await self.dispatch())["state"], "api_accepted")
            self.sender.assert_awaited_once()
        finally:
            other.close()

    async def test_cancellation_after_claim_survives_reopen_without_retry(self):
        self.enqueue()
        self.sender.side_effect = asyncio.CancelledError()
        with self.assertRaises(asyncio.CancelledError):
            await self.dispatch()
        self.outbox.close()
        self.outbox = d.Outbox(self.database)
        self.assertEqual(self.enqueue()["state"], "unknown")
        self.assertIsNone(await self.dispatch())
        self.sender.assert_awaited_once()

    async def test_receipt_persistence_failure_never_retries_successful_send(self):
        self.enqueue()
        self.outbox.connection.execute("""CREATE TRIGGER fail_receipt BEFORE UPDATE ON documents
            WHEN NEW.state = 'api_accepted' BEGIN SELECT RAISE(FAIL, 'disk failure'); END""")
        with self.assertRaises(sqlite3.Error):
            await self.dispatch()
        self.assertEqual(self.outbox.receipt(self.aid)["state"], "unknown")
        self.assertIsNone(await self.dispatch())
        self.sender.assert_awaited_once()

    async def test_corrupt_snapshot_and_private_database(self):
        self.enqueue()
        self.outbox.connection.execute("UPDATE documents SET content = ?", (b"changed",))
        self.assertEqual((await self.dispatch())["state"], "rejected")
        self.sender.assert_not_awaited()
        self.assertEqual(self.database.stat().st_mode & 0o777, 0o600)
        self.assertEqual(self.database.parent.stat().st_mode & 0o777, 0o700)
        self.database.chmod(0o666)
        with self.assertRaises(d.DocumentError):
            d.Outbox(self.database)
        self.database.chmod(0o600)

    async def test_cli_offline_enqueue_and_receipt(self):
        for option, value in (("--enqueue", str(self.marker)), ("--status", self.aid)):
            with mock.patch("builtins.print") as printer:
                self.assertEqual(d.main(["--database", str(self.database), option, value]), 0)
                self.assertEqual(json.loads(printer.call_args.args[0])["state"], "pending")
        self.sender.assert_not_awaited()
        self.sender.side_effect = TimeoutError()
        await self.dispatch()
        with mock.patch("builtins.print"):
            self.assertEqual(d.main(["--database", str(self.database), "--status", self.aid]), 2)


class TransportTests(unittest.IsolatedAsyncioTestCase):
    def setUp(self):
        self.content = "# Markdown\nEspañol.\n".encode()
        self.message = types.SimpleNamespace(
            message_id=123, chat=types.SimpleNamespace(id=42, type="private"),
            document=types.SimpleNamespace(file_id="file", file_name="report.md", file_size=len(self.content)))
        self.bot = types.SimpleNamespace(send_document=mock.AsyncMock(return_value=self.message))
        sdk_types = types.ModuleType("aiogram.types")
        class BufferedInputFile:
            def __init__(self, data, filename):
                self.data, self.filename = data, filename
        sdk_types.BufferedInputFile = BufferedInputFile
        sdk_errors = types.ModuleType("aiogram.exceptions")
        for name in ("TelegramBadRequest", "TelegramForbiddenError", "TelegramNotFound", "TelegramRetryAfter"):
            setattr(sdk_errors, name, type(name, (Exception,), {}))
        self.errors = sdk_errors
        self.patch = mock.patch.dict(sys.modules, {"aiogram.types": sdk_types, "aiogram.exceptions": sdk_errors})
        self.patch.start()
        self.addCleanup(self.patch.stop)

    async def send(self):
        return await d.send_document(self.bot, 42, "report.md", self.content, 'Informe "final" & <Markdown>')

    async def test_uses_existing_bot_with_exact_bytes_filename_plain_caption_and_timeout(self):
        self.assertEqual(await self.send(), 123)
        self.bot.send_document.assert_awaited_once()
        call = self.bot.send_document.call_args.kwargs
        self.assertEqual(call["chat_id"], 42)
        self.assertEqual(call["document"].data, self.content)
        self.assertEqual(call["document"].filename, "report.md")
        self.assertEqual(call["caption"], 'Informe "final" & <Markdown>')
        self.assertIsNone(call["parse_mode"])
        self.assertEqual(call["request_timeout"], 45)

    async def test_rejections_are_known_and_other_errors_remain_unknown(self):
        for name in ("TelegramBadRequest", "TelegramForbiddenError", "TelegramNotFound", "TelegramRetryAfter"):
            self.bot.send_document.side_effect = getattr(self.errors, name)("secret URL")
            with self.assertRaises(d.Rejected) as caught:
                await self.send()
            self.assertNotIn("secret", str(caught.exception))
        for error in (TimeoutError("secret"), ValueError("secret"), OSError("secret")):
            self.bot.send_document.side_effect = error
            with self.assertRaises(d.DocumentError) as caught:
                await self.send()
            self.assertNotIsInstance(caught.exception, d.Rejected)
            self.assertNotIn("secret", str(caught.exception))

    async def test_wrong_chat_document_or_message_id_cannot_claim_success(self):
        for obj, attribute, value in ((self.message, "message_id", True),
                                       (self.message.chat, "id", 7), (self.message.chat, "type", "group"),
                                       (self.message.document, "file_size", 0),
                                       (self.message.document, "file_name", "wrong.md")):
            old = getattr(obj, attribute)
            setattr(obj, attribute, value)
            with self.assertRaises(d.DocumentError):
                await self.send()
            setattr(obj, attribute, old)
        with self.assertRaises(d.DocumentError):
            await d.send_document(self.bot, 42, 'inject".md', self.content, "caption")


class LifecycleTests(unittest.IsolatedAsyncioTestCase):
    async def test_startup_uses_one_existing_bot_and_shutdown_cancels_pending_send(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp).resolve()
            database = root / "private" / "outbox.sqlite3"
            dp = types.SimpleNamespace(startup=mock.Mock(), shutdown=mock.Mock())
            bot = object()
            d.register(dp, database=database, user_id=42, log=logging.getLogger("documents-test"))
            start = dp.startup.register.call_args.args[0]
            stop = dp.shutdown.register.call_args.args[0]
            entered = asyncio.Event()
            async def dispatch(outbox, actual_bot, user_id, *, now):
                self.assertIs(actual_bot, bot)
                self.assertEqual(user_id, 42)
                entered.set()
                await asyncio.Event().wait()
            with mock.patch.object(d.Outbox, "dispatch_one", dispatch):
                await start(bot)
                await start(bot)
                await asyncio.wait_for(entered.wait(), 2)
                await stop()
                await stop()
            self.assertTrue(database.is_file())

    async def test_storage_failure_disables_documents_without_stopping_existing_polling(self):
        dp = types.SimpleNamespace(startup=mock.Mock(), shutdown=mock.Mock())
        log = mock.Mock()
        d.register(dp, database=Path('/nonexistent/outbox.sqlite3'), user_id=42, log=log)
        start = dp.startup.register.call_args.args[0]
        stop = dp.shutdown.register.call_args.args[0]
        with mock.patch.object(d, "Outbox", side_effect=OSError("sensitive storage detail")):
            await start(object())
            await stop()
        log.error.assert_called_once_with(
            "overlay: document outbox startup failed closed; document delivery disabled")
        log.info.assert_not_called()

    async def test_bridge_registration_passes_operator_and_data_dir_without_loading_config(self):
        import bridge_local as bl
        dp = mock.Mock()
        ctx = {"CONDUCTOR_DIR": Path('/fixture/conductor'), "log": mock.Mock()}
        with mock.patch.object(bl, "Command", lambda *names: names), \
                mock.patch.object(bl, "F", None), mock.patch.object(bl, "install_conductor_send"), \
                mock.patch.object(d, "register") as register:
            bl.register(dp, ctx, lambda message: True, authorized_user_id=42)
        register.assert_called_once_with(
            dp, database=Path('/fixture/conductor/document-outbox/outbox.sqlite3'),
            user_id=42, log=ctx["log"])


if __name__ == "__main__":
    unittest.main()
