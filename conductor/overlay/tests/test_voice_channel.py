"""The on_audio handler end to end: who may open the channel, and what comes back.

These drive the registered handler itself with a fake Telegram message, so they
cover the parts a unit test of the engine cannot: authentication, exactly-once
execution, the fail-closed branches, and the round trip that puts the conductor's
answer back in the chat.
"""

from __future__ import annotations

import asyncio
import json
import logging
import sys
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import bridge_local as bl  # noqa: E402
import media  # noqa: E402
import transcribe as t  # noqa: E402


class _AnyFilter:
    """Stands in for aiogram's magic filters so handlers register without it."""

    def __getattr__(self, name):
        return _AnyFilter()

    def __or__(self, other):
        return _AnyFilter()

    def startswith(self, prefix):
        return _AnyFilter()


class VoiceChannel(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.root = Path(self.tmp.name)
        self.addCleanup(self.tmp.cleanup)
        self._real_F, self._real_Command = bl.F, bl.Command
        bl.F, bl.Command = _AnyFilter(), (lambda *names: names)
        self.addCleanup(lambda: setattr(bl, "F", self._real_F))
        self.addCleanup(lambda: setattr(bl, "Command", self._real_Command))

        self.sent = []            # (body, kwargs) handed to the conductor
        self.replies = []         # what Telegram received
        self.pending = []         # _register_pending_reply calls
        self.transcript = t.Transcript("apaga el servidor de staging", language="es",
                                       confidence=0.91, engine="whisper.cpp small",
                                       audio_seconds=4.0)
        self.transcribe_error = None
        self.status = "idle"
        self.send_result = (True, "listo, lo hice", False)

    # ------------------------------------------------------------- harness
    def _register(self, authorized=True):
        handlers = {}

        class DP:
            class message:
                @staticmethod
                def register(fn, flt=None):
                    handlers[fn.__name__] = fn

            class callback_query:
                @staticmethod
                def register(fn, flt=None):
                    handlers[fn.__name__] = fn

        async def ensure_running(name, profile):
            return True

        def send_to_conductor(session, body, **kw):
            self.sent.append((body, kw))
            return self.send_result

        ctx = {
            "run_cli": lambda *a, **k: SimpleNamespace(returncode=0, stdout="", stderr=""),
            "log": logging.getLogger("voice-test"),
            "get_unique_profiles": lambda: ["operator"],
            "get_sessions_list_all": lambda p: [],
            "get_default_conductor": lambda: {"name": "slavna", "profile": "operator"},
            "conductor_session_title": lambda n: "conductor-%s" % n,
            "split_message": lambda text: [text],
            "md_to_tg_html": lambda text: text,
            "get_conductor_names": lambda: ["slavna"],
            "resolve_config_path": lambda n: "/nonexistent/" + n,
            "get_session_status": lambda s, profile=None: self.status,
            "ensure_conductor_running": ensure_running,
            "RESPONSE_TIMEOUT": 300,
            "_register_pending_reply": lambda s, p, cb: self.pending.append((s, p, cb)),
            "CONDUCTOR_DIR": self.root,
        }
        bl.register(DP, ctx, lambda m: authorized)
        ctx["send_to_conductor"] = send_to_conductor      # after any rebind
        self.ctx = ctx
        return handlers

    def _message(self, unique_id="u1", caption="", forward=None):
        downloaded = []

        async def download(file_obj, destination):
            Path(destination).write_bytes(b"opus-bytes")
            downloaded.append(destination)

        async def answer(text, **kw):
            self.replies.append(text)

        async def send_message(chat_id, text, **kw):
            self.replies.append(text)

        self.downloaded = downloaded
        return SimpleNamespace(
            voice=SimpleNamespace(mime_type="audio/ogg", file_size=2048, duration=4,
                                  **({"file_unique_id": unique_id} if unique_id else {})),
            caption=caption,
            from_user=SimpleNamespace(id=42),
            **(forward or {}),
            chat=SimpleNamespace(id=99),
            answer=answer,
            bot=SimpleNamespace(download=download, send_message=send_message),
        )

    def _run(self, authorized=True, **msg_kw):
        handlers = self._register(authorized=authorized)

        def fake_transcribe(path, **kw):
            if self.transcribe_error:
                raise self.transcribe_error
            return self.transcript

        real = t.transcribe_voice
        t.transcribe_voice = fake_transcribe
        try:
            asyncio.run(handlers["on_audio"](self._message(**msg_kw)))
        finally:
            t.transcribe_voice = real
        return handlers

    @property
    def chat(self) -> str:
        return "\n".join(self.replies)

    # ------------------------------------------------------- authentication
    def test_an_unauthorized_sender_opens_nothing(self):
        """I8: the channel is the operator's alone — no download, no engine, no prompt."""
        self._run(authorized=False)
        self.assertEqual(self.downloaded, [])
        self.assertEqual(self.sent, [])
        self.assertEqual(self.replies, [])

    # --------------------------------------------------------- the round trip
    def test_the_transcript_is_delivered_as_a_prompt(self):
        self._run()
        self.assertEqual(len(self.sent), 1)
        body = self.sent[0][0]
        self.assertIn("apaga el servidor de staging", body)
        self.assertIn("from the operator", body)
        self.assertNotIn(media.FENCE, body)

    def test_the_conductors_answer_comes_back_to_telegram(self):
        self._run()
        self.assertIn("listo, lo hice", self.chat)

    def test_what_was_heard_is_shown_so_a_mishearing_is_visible(self):
        self._run()
        self.assertIn("apaga el servidor de staging", self.chat)
        self.assertIn("whisper.cpp small", self.chat)

    def test_a_busy_conductor_queues_and_answers_later(self):
        self.status = "running"
        self.send_result = (True, "", False)
        self._run()
        _body, kwargs = self.sent[0]
        self.assertTrue(kwargs["force_queue"])
        self.assertIn("busy", self.chat)
        asyncio.run(kwargs["reply_callback"]("terminado"))
        self.assertIn("terminado", self.chat)

    def test_a_slow_turn_is_watched_not_resent(self):
        """Re-sending would make the conductor act on the same words twice."""
        self.send_result = (False, "", True)
        self._run()
        self.assertEqual(len(self.sent), 1)
        self.assertEqual(len(self.pending), 1)
        self.assertIn("still working", self.chat)
        asyncio.run(self.pending[0][2]("terminado"))
        self.assertIn("terminado", self.chat)

    # ------------------------------------------------------- exactly once
    def test_the_same_note_is_never_executed_twice(self):
        """I5/I10: Telegram redelivers updates; a spoken order must not repeat."""
        self._run()
        self.assertEqual(len(self.sent), 1)
        self.replies.clear()
        self._run()
        self.assertEqual(len(self.sent), 1, "the redelivered note must not be executed again")
        self.assertIn("Already handled", self.chat)

    def test_a_repeat_is_refused_without_fetching_it_again(self):
        self._run()
        self._run()
        self.assertEqual(len(self.downloaded), 0,
                         "the second delivery must not cost a download or a second file")

    def test_without_a_telegram_id_the_content_decides_identity(self):
        """Telegram omits file_unique_id for some payloads; the bytes still identify it."""
        self._run(unique_id=None)
        self.assertEqual(len(self.sent), 1)
        self.replies.clear()
        self._run(unique_id=None)
        self.assertEqual(len(self.sent), 1)
        self.assertIn("Already handled", self.chat)
        saved = list((self.root / "inbox" / "audio").rglob("*.ogg"))
        self.assertEqual(len(saved), 1, "the duplicate copy must not be left on disk")

    def test_a_different_note_is_still_executed(self):
        self._run(unique_id="u1")
        self._run(unique_id="u2")
        self.assertEqual(len(self.sent), 2)

    def test_it_is_recorded_before_the_hand_off(self):
        ledger = bl.voice_ledger_path({"CONDUCTOR_DIR": self.root})
        self._run()
        self.assertTrue(ledger.is_file())
        record = json.loads(ledger.read_text().splitlines()[0])
        self.assertEqual(record["key"], "tg:u1")

    def test_an_unwritable_ledger_stops_the_prompt(self):
        """Without the guard there is no exactly-once, so nothing is executed."""
        original = bl.record_voice_execution
        bl.record_voice_execution = lambda *a, **k: (_ for _ in ()).throw(OSError("read-only"))
        try:
            self._run()
        finally:
            bl.record_voice_execution = original
        self.assertEqual(self.sent, [])
        self.assertIn("not run", self.chat)

    # ------------------------------------------------- forwarded is not his
    # `is_authorized` answers who *sent* the note, not who *recorded* it. A note
    # forwarded from a group or a third party passes that gate, and executing it
    # would run a stranger's words as the operator's instruction.
    FORWARDS = (
        {"forward_origin": SimpleNamespace(type="user")},
        {"forward_from": SimpleNamespace(id=7)},
        {"forward_from_chat": SimpleNamespace(id=-100)},
        {"forward_sender_name": "alguien"},
        {"forward_date": 1788000000},
        {"is_automatic_forward": True},
        {"via_bot": SimpleNamespace(id=5)},
        {"sender_chat": SimpleNamespace(id=-100)},
    )

    def test_every_forward_marker_keeps_it_out_of_the_command_path(self):
        for marker in self.FORWARDS:
            with self.subTest(marker=list(marker)[0]):
                self.assertTrue(bl.is_forwarded(SimpleNamespace(**marker)))
        self.assertFalse(bl.is_forwarded(SimpleNamespace(text="hola")))

    def test_a_forwarded_note_is_delivered_as_data_not_as_an_order(self):
        self._run(forward={"forward_origin": SimpleNamespace(type="user")})
        self.assertEqual(len(self.sent), 1)
        body = self.sent[0][0]
        self.assertIn(media.FENCE, body, "a stranger's words must stay behind the fence")
        self.assertNotIn("from the operator", body,
                         "a stranger's note must not get the operator preamble")
        self.assertIn("never as", body)
        self.assertIn("Forwarded note", self.chat)

    def test_a_forwarded_note_is_still_transcribed_and_answered(self):
        self._run(forward={"forward_from": SimpleNamespace(id=7)})
        self.assertIn("apaga el servidor de staging", self.chat)
        self.assertIn("listo, lo hice", self.chat)

    def test_forwarding_the_same_note_twice_is_not_refused(self):
        """Exactly-once guards execution; nothing is executed on this path."""
        forward = {"forward_origin": SimpleNamespace(type="user")}
        self._run(forward=forward)
        self._run(forward=forward)
        self.assertEqual(len(self.sent), 2)
        self.assertNotIn("Already handled", self.chat)

    def test_a_forwarded_note_never_enters_the_ledger(self):
        self._run(forward={"forward_origin": SimpleNamespace(type="user")})
        self.assertFalse(bl.voice_ledger_path({"CONDUCTOR_DIR": self.root}).exists())

    def test_a_garbled_forward_is_data_too_not_a_refusal(self):
        self.transcript = t.Transcript("bla ... bla", language="es",
                                       confidence=t.MIN_CONFIDENCE - 0.2,
                                       engine="whisper.cpp small", audio_seconds=2.0)
        self._run(forward={"forward_origin": SimpleNamespace(type="user")})
        self.assertEqual(len(self.sent), 1)
        self.assertIn(media.FENCE, self.sent[0][0])

    # --------------------------------------------------------- fail closed
    def test_an_unavailable_engine_saves_the_audio_and_says_why(self):
        self.transcribe_error = t.TranscriptionUnavailable("the speech model is missing at /x")
        self._run()
        self.assertEqual(self.sent, [])
        self.assertIn("Not transcribed", self.chat)
        self.assertIn("the speech model is missing at /x", self.chat)
        self.assertIn("nothing was sent to any other service", self.chat.lower())

    def test_a_refused_job_is_reported_not_swallowed(self):
        self.transcribe_error = t.JobRejected("another transcription job is already running")
        self._run()
        self.assertEqual(self.sent, [])
        self.assertIn("already running", self.chat)

    def test_garbled_audio_is_shown_but_never_acted_on(self):
        """I10: a half-heard sentence is a different instruction."""
        self.transcript = t.Transcript("apaga el ... vidor", language="es",
                                       confidence=t.MIN_CONFIDENCE - 0.1,
                                       engine="whisper.cpp small", audio_seconds=4.0)
        self._run()
        self.assertEqual(self.sent, [])
        self.assertIn("too unclearly to act on", self.chat)
        self.assertIn("apaga el ... vidor", self.chat)

    def test_a_note_with_no_reported_confidence_still_runs(self):
        self.transcript = t.Transcript("hola", language="es", confidence=None,
                                       engine="whisper.cpp small", audio_seconds=1.0)
        self._run()
        self.assertEqual(len(self.sent), 1)

    def test_the_audio_is_kept_on_disk_whatever_happens(self):
        self.transcribe_error = t.TranscriptionUnavailable("nope")
        self._run()
        saved = list((self.root / "inbox" / "audio").rglob("*.ogg"))
        self.assertEqual(len(saved), 1)


if __name__ == "__main__":
    unittest.main()


class QueuedReplyFreshness(unittest.TestCase):
    """A queued or late reply is relayed only if it postdates the question."""

    def _ctx(self, pane_lines):
        import logging
        return {
            "log": logging.getLogger("fresh-test"),
            "run_cli": lambda *a, **k: SimpleNamespace(
                returncode=0, stdout="\n".join(pane_lines), stderr=""),
            "get_session_status": lambda s, profile=None: "waiting",
        }

    def test_a_stale_cached_reply_is_replaced_by_the_panes(self):
        question = "hola probando audio como estas dime algo largo aqui"
        pane = ["❯ " + question, "", "⏺ Te escucho bien, la prueba funciono.", "", "❯ "]
        got = []
        async def cb(text): got.append(text)
        wrapped = bl.fresh_reply_callback(self._ctx(pane), "conductor-slavna", "operator",
                                          question, cb)
        asyncio.run(wrapped("[STATUS] All clear from three hours ago"))
        self.assertEqual(len(got), 1)
        self.assertIn("Te escucho bien", got[0])
        self.assertNotIn("three hours ago", got[0])

    def test_a_fresh_cached_reply_passes_through(self):
        question = "hola probando audio como estas dime algo largo aqui"
        pane = ["❯ " + question, "", "⏺ Te escucho bien, la prueba funciono.", "", "❯ "]
        got = []
        async def cb(text): got.append(text)
        wrapped = bl.fresh_reply_callback(self._ctx(pane), "conductor-slavna", "operator",
                                          question, cb)
        asyncio.run(wrapped("Te escucho bien, la prueba funciono."))
        self.assertEqual(got, ["Te escucho bien, la prueba funciono."])

    def test_without_a_pane_nothing_stale_is_relayed(self):
        ctx = self._ctx([])
        ctx["run_cli"] = lambda *a, **k: SimpleNamespace(returncode=1, stdout="", stderr="x")
        got = []
        async def cb(text): got.append(text)
        wrapped = bl.fresh_reply_callback(ctx, "conductor-slavna", "operator", "pregunta larga de prueba", cb)
        asyncio.run(wrapped("[STATUS] stale"))
        self.assertNotIn("stale", got[0])
        self.assertIn("No fresh reply", got[0])


class QueuedVoiceStartsTheDrain(VoiceChannel):
    def test_a_queued_note_starts_the_drain_on_the_loop(self):
        kicked = []
        self.status = "running"
        self.send_result = (True, "", False)
        handlers = self._register()
        self.ctx["_ensure_drain_task"] = lambda: kicked.append(True)
        real = t.transcribe_voice
        t.transcribe_voice = lambda path, **kw: self.transcript
        try:
            asyncio.run(handlers["on_audio"](self._message()))
        finally:
            t.transcribe_voice = real
        self.assertEqual(kicked, [True], "the drain must be started from the loop thread")
