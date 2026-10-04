#!/usr/bin/env python3
"""Deterministic backup/publication guards with temporary, neutral SQLite data."""

from contextlib import closing
import json
import os
from pathlib import Path
import re
import sqlite3
import subprocess
import tempfile
import unittest
from unittest.mock import patch

from scripts import backup_db as subject
from ..support import ROOT

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
                            'retry_after_ms': 123, 'at': '2026-02-02T02:40:00.123456789Z',
                            'provider_request_id': SENTINEL, 'provider_server': SENTINEL,
                            'provider_model': SENTINEL, 'processing_ms': 321,
                            'rate_limit_remaining': 4, 'rate_limit_limit': 5,
                            'response_headers': {'X-Request-Id': [SENTINEL]}},
                           {'status_code': 0, 'error_type': 'transport', 'at': '2026-02-02T02:40:01Z'},
                           {'status_code': 503, 'error_type': SENTINEL, 'error_code': SENTINEL,
                            'error_msg': SENTINEL, 'at': '2026-02-02T02:40:02-04:00',
                            'provider_model': SENTINEL},
                       ]), tool_names=json.dumps([SENTINEL, SENTINEL, 'other-private-tool']))
        record.update(changes)
        return record

    def insert(self, connection, record):
        connection.execute('INSERT INTO requests VALUES (' + ','.join('?' for _ in self.columns) + ')',
                           [record[name] for name in self.columns])
        connection.commit()

    def ancestor_source_db(self, drop):
        """A migration-ancestor source: the current schema minus drop columns."""
        connection = sqlite3.connect(self.source)
        self.addCleanup(connection.close)
        columns = [name for name in self.columns if name not in frozenset(drop)]
        definitions = [name + ' ' + subject.FIELD_TYPES[name]
                       + (' PRIMARY KEY' if name == 'id' else '') for name in columns]
        connection.execute('CREATE TABLE requests (' + ','.join(definitions) + ')')
        return connection, columns

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
            # Attempt metadata follows the record-level policy: the numeric
            # trio publishes verbatim, provider_model aliases with the model
            # kind, and the request id, server and header map are discarded.
            self.assertEqual((attempts[0]['processing_ms'], attempts[0]['rate_limit_remaining'],
                              attempts[0]['rate_limit_limit']), (321, 4, 5))
            self.assertEqual(attempts[0]['provider_model'], attempts[2]['provider_model'])
            self.assertNotIn('provider_model', attempts[1])
            for dropped in subject.ATTEMPT_DROPPED_FIELDS:
                self.assertNotIn(dropped, attempts[0])
                self.assertNotIn(dropped, attempts[1])
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

    def test_verifier_rejects_served_attempt_response_metadata(self):
        connection = self.source_db()
        self.insert(connection, self.record())
        self.copy()
        with closing(sqlite3.connect(self.destination)) as connection:
            connection.execute('UPDATE requests SET attempts=?',
                               ('[{"status_code":429,"at":"2026-01-01T00:00:00Z",'
                                '"provider_request_id":"req_leaked_value"}]',))
            connection.commit()
        with self.assertRaisesRegex(ValueError, 'unredacted attempt response metadata') as caught:
            subject.verify_docs_copy(self.destination)
        self.assertNotIn('req_leaked_value', str(caught.exception))

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

    def test_migration_ancestor_sources_derive_the_zero_backfill(self):
        # An operator's local history can predate the latest migration
        # columns: store.go appends each with INTEGER NOT NULL DEFAULT 0, so
        # the derive accepts the ancestor and projects the migration's own
        # backfill - identical to migrating the source first and deriving
        # afterwards. The destination always carries the complete schema.
        full, intermediate, single = (subject.MIGRATION_ZERO_COLUMNS,
                                      subject.MIGRATION_ZERO_COLUMNS[:2],
                                      subject.MIGRATION_ZERO_COLUMNS[-1:])
        for drop in (full, intermediate, single):
            with self.subTest(drop=drop):
                connection, columns = self.ancestor_source_db(drop)
                record = self.record()
                connection.execute('INSERT INTO requests VALUES (' + ','.join('?' for _ in columns) + ')',
                                   [record[name] for name in columns])
                connection.commit()
                self.assertEqual(self.copy()['records'], 1)
                with closing(sqlite3.connect(self.destination)) as served:
                    served_columns = [row[1] for row in served.execute('PRAGMA table_xinfo(requests)')]
                    self.assertEqual(set(served_columns), set(subject.FIELD_TYPES))
                    for name in drop:
                        self.assertEqual(
                            served.execute('SELECT "' + name + '" FROM requests').fetchone()[0], 0,
                            name + ' must be the migration backfill, never the dropped source value')
                connection.close()
                self.source.unlink()
                self.destination.unlink()

    def test_migration_zero_columns_are_the_store_lineage_tail(self):
        # The derive accepts a migration-ancestor source by projecting each
        # MIGRATION_ZERO_COLUMNS name as exactly the migration's zero
        # backfill, and that projection is only truthful while the tuple
        # equals the migration's own lineage: the tail of migrate's needed
        # slice in internal/storage/store.go. This is the mechanical
        # contract between the two sides: a migration column added,
        # removed or reordered must move scripts/backup_db.py's
        # MIGRATION_ZERO_COLUMNS and the Go needed-slice tail in the same
        # change, or this test fails. The window is the slice tail, never a
        # DDL-shape filter over the whole slice: older entries share the
        # zero-DDL shape (rate_limit_remaining) without being lineage
        # columns, and req_param_presence concatenates a DDL constant.
        source = (ROOT / 'internal' / 'storage' / 'store.go').read_text(encoding='utf-8')
        declaration = 'needed := []struct{ name, ddl string }{'
        head = source.find(declaration)
        self.assertNotEqual(head, -1,
                             'migrate\'s needed slice declaration not found in '
                             'internal/storage/store.go; if the migration list moved, '
                             'update this guard and MIGRATION_ZERO_COLUMNS together')
        entries = []
        for line in source[head + len(declaration):].splitlines():
            stripped = line.strip()
            if not stripped:
                continue
            if stripped == '}':
                break
            match = re.fullmatch(r'\{"([a-z0-9_]+)", (.+)\},', stripped)
            self.assertIsNotNone(match, 'unparsed needed-slice entry: ' + stripped)
            entries.append(match.groups())
        tail = entries[-len(subject.MIGRATION_ZERO_COLUMNS):]
        self.assertEqual([name for name, _ in tail], list(subject.MIGRATION_ZERO_COLUMNS),
                         'scripts/backup_db.py MIGRATION_ZERO_COLUMNS no longer equals the '
                         'tail of migrate\'s needed slice in internal/storage/store.go; both '
                         'sides must move together: add, remove or reorder the migration '
                         'column in the Go slice and this tuple in the same change')
        for name, ddl in tail:
            self.assertEqual(
                ddl, '"ALTER TABLE requests ADD COLUMN ' + name + ' INTEGER NOT NULL DEFAULT 0"',
                name + ' must migrate with exactly the zero backfill the derive projects; '
                'keep the Go needed-slice entry and scripts/backup_db.py\'s '
                'MIGRATION_ZERO_COLUMNS in sync')

    def test_non_migration_column_loss_still_fails_closed(self):
        # Only the zero-backfilled migration columns may be absent. Losing
        # any other column - alone or beside a migration column - is an
        # unrecognized schema and the derive refuses.
        for drop in (('cost',), ('cost',) + subject.MIGRATION_ZERO_COLUMNS[:1]):
            with self.subTest(drop=drop):
                connection, _ = self.ancestor_source_db(drop)
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
            {'attempts': '[{"status_code":429,"at":"2026-01-01T00:00:00Z","processing_ms":"fast"}]'},
            {'attempts': '[{"status_code":429,"at":"2026-01-01T00:00:00Z","rate_limit_remaining":null}]'},
            {'attempts': '[{"status_code":429,"at":"2026-01-01T00:00:00Z","response_headers":{"X-Request-Id":[7]}}]'},
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
