"""Run this optional offline check under the installed bridge venv (no config)."""
import asyncio
import importlib.util
import unittest
import sys
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import documents

async def check():
    from aiogram import Bot
    from aiogram.client.session.aiohttp import AiohttpSession
    from aiogram.methods import SendDocument
    from aiogram.types import Message
    content = '# Slavna transport contract\n\nTexto español.\n'.encode()
    session = AiohttpSession()
    # Syntactically valid fixture token, never sourced from configuration.
    bot = Bot('123456:' + 'x' * 32, session=session)
    captured = []
    async def make_request(actual_bot, method, timeout=None):
        assert actual_bot is bot and isinstance(method, SendDocument)
        assert method.chat_id == 42 and method.parse_mode is None and timeout == 45
        assert method.document.filename == 'test.md'
        assert b''.join([chunk async for chunk in method.document.read(bot)]) == content
        form = session.build_form_data(bot, method)
        fields = {field[0]['name']: field for field in form._fields}
        attachment = fields['document'][2].split('attach://', 1)[1]
        assert attachment in fields
        file_field = fields[attachment]
        assert file_field[0]['filename'] == 'test.md'
        assert b''.join([chunk async for chunk in file_field[2]]) == content
        assert fields['caption'][2] == 'Prueba Markdown'
        assert 'parse_mode' not in fields
        captured.append(method)
        return Message.model_validate({'message_id': 123, 'date': 0,
            'chat': {'id': 42, 'type': 'private'},
            'document': {'file_id': 'file', 'file_unique_id': 'unique', 'file_name': 'test.md', 'file_size': len(content)}})
    try:
        with mock.patch.object(session, 'make_request', side_effect=make_request):
            result = await documents.send_document(bot, 42, 'test.md', content, 'Prueba Markdown')
        assert result == 123 and len(captured) == 1
    finally:
        await session.close()
@unittest.skipUnless(importlib.util.find_spec("aiogram"), "optional real SDK; CI installs pinned aiogram on Python 3.12")
class RealSDKTest(unittest.IsolatedAsyncioTestCase):
    async def test_exact_multipart_through_existing_bot_without_network(self):
        await check()

if __name__ == "__main__":
    unittest.main()
