#!/usr/bin/env python3
"""Deterministic backup/publication guards with temporary, neutral SQLite data."""

from contextlib import closing
import json
import os
from pathlib import Path
import sqlite3
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import backup_db as subject

ROOT = Path(__file__).resolve().parents[1]
SENTINEL = 'private-fixture-content-never-publish'


class BackupTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix='millivolt-backup-test-')
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.source = self.root / 'source.db'
        self.destination = self.root / 'docs.db'
        self.columns = sorted(subject.FIELD_TYPES)

    def source_db(self):
        connection = sqlite3.connect(self.source)
        self.addCleanup(connection.close)
        # Unit fixtures exercise the publication policy. The Go integration
        # test independently checks it against Store.Open's canonical schema.
        definitions = [name + ' ' + subject.FIELD_TYPES[name]
                       + (' PRIMARY KEY' if name == 'id' else '') for name in self.columns]
        connection.execute('CREATE TABLE requests (' + ','.join(definitions) + ')')
        return connection

    def record(self, **changes):
        record = {name: (SENTINEL + '-' + name if kind == 'TEXT' else 0.25 if kind == 'REAL' else 7)
                  for name, kind in subject.FIELD_TYPES.items()}
        record.update(id='original-request', started_at=1770000000000, status_code=200,
                      parent_conversation_id='', req_param_presence=-1,
                      attempts=json.dumps([
                          {'status_code': 429, 'error_type': 'rate_limit', 'error_msg': SENTINEL,
                           'retry_after_ms': 123, 'at': '2026-02-02T02:40:00.123456789Z'},
                          {'status_code': 0, 'error_type': 'transport', 'at': '2026-02-02T02:40:01Z'},
                          {'status_code': 503, 'error_type': SENTINEL, 'error_code': SENTINEL,
                           'error_msg': SENTINEL, 'at': '2026-02-02T02:40:02-04:00'},
                      ]), tool_names=json.dumps([SENTINEL, SENTINEL, 'other-private-tool']))
        record.update(changes)
        return record

    def insert(self, connection, record):
        connection.execute('INSERT INTO requests VALUES (' + ','.join('?' for _ in self.columns) + ')',
                           [record[name] for name in self.columns])
        connection.commit()

    def copy(self):
        subject.backup(self.source, self.destination, docs_safe=True)
        return subject.verify_docs_copy(self.destination)

    def rows(self):
        with closing(sqlite3.connect(self.destination)) as connection:
            connection.row_factory = sqlite3.Row
            return [dict(row) for row in connection.execute('SELECT * FROM requests ORDER BY rowid')]

    def test_preserves_metrics_memberships_and_global_relationships_without_content(self):
        connection = self.source_db()
        original = [
            self.record(id='a', conversation_id='parent', model='shared-model', provider_model='shared-model'),
            self.record(id='b', conversation_id='child', parent_conversation_id='parent', status_code=429),
            self.record(id='c', client='another-client', key_hash='another-key',
                        conversation_id='parent', parent_conversation_id='missing-parent', status_code=499),
        ]
        for record in original:
            self.insert(connection, record)
        connection.executescript('''
            CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT);
            CREATE TABLE request_debug (payload TEXT);
            CREATE TABLE unrelated (content TEXT);
            CREATE TRIGGER secret_trigger AFTER INSERT ON unrelated BEGIN SELECT 'private-trigger'; END;
        ''')
        for table in ('request_debug', 'unrelated'):
            connection.execute('INSERT INTO ' + table + ' VALUES (?)', (SENTINEL,))
        connection.execute('INSERT INTO meta VALUES (?, ?)', ('paused_keys', SENTINEL))
        connection.commit()
        summary = self.copy()
        rows = self.rows()
        self.assertEqual(summary['records'], 3)
        self.assertEqual(summary['parent_declarations'], 2)
        self.assertEqual(summary['cost'], 0.75)
        self.assertEqual(summary['input_tokens'], 21)
        self.assertEqual(summary['min_started_ms'], original[0]['started_at'])
        self.assertNotIn(SENTINEL.encode(), self.destination.read_bytes())
        self.assertNotIn(b'private-trigger', self.destination.read_bytes())
        self.assertEqual(self.destination.stat().st_mode & 0o777, 0o600)
        self.assertEqual(rows[0]['conversation_id'], rows[1]['parent_conversation_id'])
        self.assertEqual(rows[0]['conversation_id'], rows[2]['conversation_id'])
        self.assertEqual(rows[0]['key_hash'], rows[1]['key_hash'])
        self.assertNotEqual(rows[0]['key_hash'], rows[2]['key_hash'])
        self.assertEqual(rows[0]['model'], rows[0]['provider_model'])
        for before, after in zip(original, rows, strict=True):
            for field in subject.INTEGER_FIELDS | subject.REAL_FIELDS:
                self.assertEqual(before[field], after[field], field)
            self.assertTrue(all(after[field] == '' for field in subject.EMPTY_FIELDS))
            self.assertEqual(after['debug'], 0)
            names = json.loads(after['tool_names'])
            self.assertEqual(names[0], names[1])
            self.assertNotEqual(names[0], names[2])
            attempts = json.loads(after['attempts'])
            self.assertEqual([a['status_code'] for a in attempts], [429, 0, 503])
            self.assertEqual([a['error_type'] for a in attempts[:2]], ['rate_limit', 'transport'])
            self.assertEqual(attempts[0]['error_msg'], attempts[2]['error_msg'])
            self.assertEqual(attempts[0]['at'], json.loads(before['attempts'])[0]['at'])
        self.assertEqual(connection.execute('SELECT key_hash FROM requests ORDER BY rowid').fetchone()[0],
                         original[0]['key_hash'])
        second = self.root / 'second.db'
        subject.backup(self.source, second, docs_safe=True)
        with closing(sqlite3.connect(second)) as copy:
            self.assertNotEqual(copy.execute('SELECT key_hash FROM requests LIMIT 1').fetchone()[0], rows[0]['key_hash'])

    def test_online_backup_includes_committed_wal_only_and_default_remains_private(self):
        connection = self.source_db()
        connection.execute('PRAGMA journal_mode=WAL')
        connection.execute('PRAGMA wal_autocheckpoint=0')
        self.insert(connection, self.record())
        self.assertTrue(Path(str(self.source) + '-wal').exists())
        connection.execute('INSERT INTO requests SELECT ' + ','.join(
            "'uncommitted'" if name == 'id' else name for name in self.columns) + ' FROM requests')
        self.assertEqual(self.copy()['records'], 1)
        raw = self.root / 'raw.db'
        subject.backup(self.source, raw)
        with closing(sqlite3.connect(raw)) as snapshot:
            self.assertEqual(snapshot.execute('SELECT COUNT(*) FROM requests').fetchone()[0], 1)
            self.assertEqual(snapshot.execute('SELECT prompt_preview FROM requests').fetchone()[0],
                             self.record()['prompt_preview'])
        with self.assertRaises(ValueError):
            subject.verify_docs_copy(raw)

    def test_empty_and_legacy_null_arrays(self):
        connection = self.source_db()
        summary = self.copy()
        self.assertEqual(summary['records'], 0)
        self.assertIsNone(summary['min_started_ms'])
        self.destination.unlink()
        self.insert(connection, self.record(attempts='', tool_names='', key_hash='', error_type=''))
        self.copy()
        row = self.rows()[0]
        self.assertEqual((row['attempts'], row['tool_names'], row['key_hash'], row['error_type']),
                         ('null', 'null', '', ''))

    def test_discarded_invalid_text_never_enters_the_decoder(self):
        connection = self.source_db()
        self.insert(connection, self.record())
        for name in subject.EMPTY_FIELDS:
            connection.execute('UPDATE requests SET ' + name + '=CAST(? AS TEXT)',
                               (SENTINEL.encode() + b'\xff',))
        connection.execute("UPDATE requests SET debug=CAST(X'ff' AS TEXT)")
        connection.commit()
        original_readonly = subject._readonly

        def guarded_readonly(path):
            connection = original_readonly(path)
            if path.name == 'snapshot.db':
                def authorize(action, table, column, _database, _trigger):
                    if action == sqlite3.SQLITE_READ and table == 'requests' and (column in subject.EMPTY_FIELDS or column == 'debug'):
                        return sqlite3.SQLITE_DENY
                    return sqlite3.SQLITE_OK
                connection.set_authorizer(authorize)
            return connection

        with patch.object(subject, '_readonly', side_effect=guarded_readonly):
            self.copy()
        self.assertNotIn(SENTINEL.encode(), self.destination.read_bytes())
        row = self.rows()[0]
        self.assertTrue(all(row[name] == '' for name in subject.EMPTY_FIELDS))
        self.assertEqual(row['debug'], 0)

    def test_invalid_utf8_aliases_remain_distinct_and_consistent(self):
        connection = self.source_db()
        for name in ('a', 'b', 'a-again'):
            self.insert(connection, self.record(id=name))
            suffix = b'\xff' if name != 'b' else b'\xfe'
            for field in (*subject.ALIASED_FIELDS, 'key_hash'):
                connection.execute('UPDATE requests SET ' + field + '=CAST(? AS TEXT) WHERE id=?',
                                   (SENTINEL.encode() + suffix, name))
        connection.commit()
        self.copy()
        rows = self.rows()
        for field in (*subject.ALIASED_FIELDS, 'key_hash'):
            self.assertEqual(rows[0][field], rows[2][field], field)
            self.assertNotEqual(rows[0][field], rows[1][field], field)
        self.assertEqual(rows[0]['conversation_id'], rows[0]['parent_conversation_id'])
        self.assertEqual(rows[0]['model'], rows[0]['provider_model'])
        self.assertNotIn(SENTINEL.encode(), self.destination.read_bytes())

    def test_structured_fields_and_numeric_text_fail_without_disclosing_values(self):
        connection = self.source_db()
        for field in ('attempts', 'tool_names', 'input_tokens'):
            with self.subTest(field=field):
                connection.execute('DELETE FROM requests')
                self.insert(connection, self.record())
                connection.execute('UPDATE requests SET ' + field + '=CAST(? AS TEXT)',
                                   (SENTINEL.encode() + b'\xff',))
                connection.commit()
                with self.assertRaises(ValueError) as caught:
                    self.copy()
                self.assertNotIn(SENTINEL, str(caught.exception))
                self.assertFalse(self.destination.exists())

    def test_verifier_rejects_invalid_utf8_without_echoing_cell_content(self):
        connection = self.source_db()
        self.insert(connection, self.record())
        self.copy()
        with closing(sqlite3.connect(self.destination)) as connection:
            connection.execute('UPDATE requests SET provider=CAST(? AS TEXT)', (SENTINEL.encode() + b'\xff',))
            connection.commit()
        with self.assertRaisesRegex(ValueError, 'invalid UTF-8 text') as caught:
            subject.verify_docs_copy(self.destination)
        self.assertNotIn(SENTINEL, str(caught.exception))

    def test_unknown_source_columns_including_generated_fail_closed(self):
        for ddl in ('ALTER TABLE requests ADD COLUMN future TEXT',
                    "ALTER TABLE requests ADD COLUMN future TEXT AS ('private-generated')"):
            with self.subTest(ddl=ddl):
                connection = self.source_db()
                connection.execute(ddl)
                connection.commit()
                with self.assertRaisesRegex(ValueError, 'unrecognized table columns'):
                    self.copy()
                self.assertFalse(self.destination.exists())
                self.assertFalse(list(self.root.glob('millivolt-docs-backup-*')))
                connection.close()
                self.source.unlink()

    def test_malformed_values_fail_closed_and_remove_only_new_files(self):
        connection = self.source_db()
        cases = [
            {'input_tokens': SENTINEL}, {'input_tokens': None}, {'cost': float('inf')},
            {'provider': sqlite3.Binary(b'not-text')}, {'id': ''},
            {'attempts': '{invalid'}, {'attempts': '[] trailing'}, {'attempts': '[NaN]'},
            {'attempts': '[{"status_code":429,"at":"2026-01-01T00:00:00Z","unknown":"secret"}]'},
            {'attempts': '[{"status_code":true,"at":"2026-01-01T00:00:00Z"}]'},
            {'attempts': '[{"status_code":429,"status_code":200,"at":"2026-01-01T00:00:00Z"}]'},
            {'attempts': '[{"status_code":429,"at":"2026-99-01T00:00:00Z"}]'},
            {'attempts': '[{"status_code":429,"at":"secret timestamp"}]'},
            {'attempts': '[{"status_code":429,"at":"2026-01-01T00:00:00Z","error_msg":123}]'},
            {'tool_names': '{"secret":1}'}, {'tool_names': '[1]'},
        ]
        for changes in cases:
            with self.subTest(fields=list(changes)):
                connection.execute('DELETE FROM requests')
                self.insert(connection, self.record(**changes))
                with self.assertRaises(ValueError) as caught:
                    self.copy()
                self.assertNotIn(SENTINEL, str(caught.exception))
                self.assertFalse(self.destination.exists())
                self.assertFalse(list(self.root.glob('docs.db-*')))
                self.assertFalse(list(self.root.glob('millivolt-docs-backup-*')))

    def test_wrong_schema_types_views_and_partial_write_failure(self):
        for ddl in ('ALTER TABLE requests DROP COLUMN cost; ALTER TABLE requests ADD COLUMN cost NUMERIC',
                    'ALTER TABLE requests RENAME TO original; CREATE VIEW requests AS SELECT * FROM original'):
            with self.subTest(ddl=ddl):
                connection = self.source_db()
                connection.executescript(ddl)
                with self.assertRaises(ValueError):
                    self.copy()
                self.assertFalse(self.destination.exists())
                connection.close()
                self.source.unlink()
        connection = self.source_db()
        self.insert(connection, self.record(id='valid'))
        self.insert(connection, self.record(id='invalid', cost=float('inf')))
        with self.assertRaises(ValueError):
            self.copy()
        self.assertEqual(sorted(path.name for path in self.root.iterdir()), ['source.db'])

    def test_existing_destinations_and_symlinks_are_never_overwritten(self):
        self.source_db()
        self.destination.write_bytes(b'keep existing file')
        for docs_safe in (False, True):
            with self.assertRaises((FileExistsError, ValueError)):
                subject.backup(self.source, self.destination, docs_safe=docs_safe)
        self.assertEqual(self.destination.read_bytes(), b'keep existing file')
        self.destination.unlink()
        self.destination.symlink_to(self.root / 'missing-target')
        for docs_safe in (False, True):
            with self.assertRaises((FileExistsError, ValueError)):
                subject.backup(self.source, self.destination, docs_safe=docs_safe)
        self.assertTrue(self.destination.is_symlink())
        self.assertFalse((self.root / 'missing-target').exists())
        with self.assertRaises(ValueError):
            subject.verify_docs_copy(self.destination)

    def test_private_intermediate_permissions_and_cleanup_on_failure(self):
        self.source_db()

        def reject(snapshot, _destination):
            self.assertEqual(snapshot.stat().st_mode & 0o777, 0o600)
            self.assertEqual(snapshot.parent.stat().st_mode & 0o777, 0o700)
            raise RuntimeError('fixture failure')

        with patch.object(subject, '_derive', side_effect=reject):
            with self.assertRaisesRegex(RuntimeError, 'fixture failure'):
                self.copy()
        self.assertFalse(self.destination.exists())
        self.assertFalse(list(self.root.glob('millivolt-docs-backup-*')))
        with self.assertRaises(FileNotFoundError):
            subject.backup(self.root / 'absent.db', self.destination, docs_safe=True)
        self.assertFalse(self.destination.exists())

    def test_verifier_rejects_changed_rows_metadata_and_schema(self):
        connection = self.source_db()
        self.insert(connection, self.record())
        mutations = [
            "UPDATE requests SET provider='private provider'",
            "UPDATE requests SET cost=999",
            "UPDATE requests SET prompt_preview='secret'",
            "UPDATE requests SET debug=1",
            "UPDATE requests SET attempts='[{\"status_code\":429,\"at\":\"2026-01-01T00:00:00Z\",\"error_msg\":\"secret\"}]'",
            "DELETE FROM meta",
            "INSERT INTO meta VALUES ('paused_keys','secret')",
            "INSERT INTO meta VALUES ('attempts_repaired','secret')",
            "ALTER TABLE requests ADD COLUMN future TEXT",
            "ALTER TABLE meta ADD COLUMN future TEXT",
            "CREATE TABLE future (content TEXT)",
            "CREATE VIEW future AS SELECT 'secret'",
            "CREATE INDEX future ON requests(client)",
            "CREATE TRIGGER future AFTER INSERT ON requests BEGIN SELECT 'secret'; END",
            "CREATE TABLE request_debug (id TEXT PRIMARY KEY,session_id TEXT,captured_at INTEGER,expires_at INTEGER,payload TEXT); INSERT INTO request_debug VALUES ('id','session',0,0,'secret')",
            "CREATE TABLE request_projection_state (singleton INTEGER PRIMARY KEY,epoch INTEGER,high_rowid INTEGER); INSERT INTO request_projection_state VALUES (1,'secret',1)",
        ]
        for sql in mutations:
            with self.subTest(sql=sql):
                self.copy()
                with closing(sqlite3.connect(self.destination)) as edited:
                    edited.executescript(sql)
                with self.assertRaises(ValueError):
                    subject.verify_docs_copy(self.destination)
                self.destination.unlink()

    def test_provenance_types_are_strict(self):
        self.source_db()
        self.copy()
        with closing(sqlite3.connect(self.destination)) as connection:
            marker = json.loads(connection.execute('SELECT value FROM meta').fetchone()[0])
            marker['summary']['records'] = False
            connection.execute('UPDATE meta SET value=?', (json.dumps(marker, separators=(',', ':')),))
            connection.commit()
        with self.assertRaises(ValueError):
            subject.verify_docs_copy(self.destination)


class DevelopmentCopyModeTest(unittest.TestCase):
    def test_invalid_or_impossible_copy_modes_stop_before_build(self):
        # Use a separate script checkout and a failing fake Go tool. Never
        # reach the real lifecycle, database, config or user's process.
        with tempfile.TemporaryDirectory(prefix='millivolt-dev-mode-test-') as tmp:
            root = Path(tmp)
            (root / 'scripts').mkdir()
            (root / 'tools').mkdir()
            script = root / 'scripts/dev.sh'
            script.write_bytes((ROOT / 'scripts/dev.sh').read_bytes())
            (root / 'proxy.example.yaml').write_text('{}\n')
            go = root / 'tools/go'
            go.write_text('#!/bin/sh\necho unexpected-build >&2\nexit 71\n')
            go.chmod(0o700)
            for mode, database in [('invalid', 'none'), ('2', 'none'), ('redacted', 'none'),
                                   ('1', 'none'), ('redacted', '/tmp/millivolt/millivolt-dev-mode-test.db')]:
                with self.subTest(mode=mode, database=database):
                    env = {key: value for key, value in os.environ.items() if not key.startswith('DEV_')}
                    env.update(PATH=str(root / 'tools') + os.pathsep + os.environ['PATH'],
                               DEV_COPY_DB=mode, DEV_DB=database, DEV_PORT='18087')
                    result = subprocess.run(['bash', str(script)], env=env, text=True,
                                            capture_output=True, timeout=10, check=False)
                    self.assertNotEqual(result.returncode, 0)
                    self.assertNotIn('unexpected-build', result.stderr)
                    self.assertIn('DEV_COPY_DB', result.stderr)


if __name__ == '__main__':
    unittest.main()
