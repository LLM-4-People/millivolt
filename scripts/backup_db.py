#!/usr/bin/env python3
"""Consistent SQLite snapshot, including committed WAL pages, to a NEW file.

The source is opened read-only. Existing destinations are never overwritten.
Used by dev.sh; also useful for explicit operator backups. --docs-safe derives
a fresh metrics-only database, with no source content, schema SQL or free pages.
"""
from contextlib import closing
from pathlib import Path
import argparse
import datetime
import hashlib
import hmac
import json
import math
import os
import re
import secrets
import sqlite3
import tempfile

# Internal backup page chunk; allows the source writer to run between steps.
BACKUP_PAGES = 256

# This explicit publication policy is intentionally not inferred from SQLite
# affinity: a future column must be reviewed before its values can be exposed.
INTEGER_FIELDS = frozenset('''
stream status_code started_at duration_ms ttft_ms first_token_at last_token_at
tool_calls input_tokens output_tokens total_tokens cache_read_tokens
cache_write_tokens reasoning_tokens client_disconnected rate_limit_remaining
rate_limit_limit turns_user turns_assistant turns_tool answer_tokens first_answer_at
gen_tokens had_answer_content processing_ms queue_wait_ms rate_limited retries
retry_after_ms req_max_tokens req_tools_count req_n req_stop req_logprobs req_seed
req_parallel_tools req_logit_bias req_top_logprobs req_thinking req_metadata_keys
req_stream_opts final_attempt_at chars_system chars_user chars_assistant chars_tool
images attachments req_param_presence
'''.split())
REAL_FIELDS = frozenset('''
cost gen_tps overall_tps req_temperature req_top_p req_presence_pen req_frequency_pen
'''.split())
ALIASED_FIELDS = {
    'provider': 'provider', 'model': 'model', 'provider_model': 'model',
    'client': 'client', 'conversation_id': 'conversation',
    'parent_conversation_id': 'conversation', 'finish_reason': 'finish',
    'error_type': 'error type', 'error_code': 'error code', 'error_msg': 'error detail',
}
EMPTY_FIELDS = frozenset('''
user_agent client_ip client_lang response_headers method path prompt_preview
response_preview provider_request_id provider_server req_tool_choice
req_response_format req_service_tier last_turn_role client_meta req_reasoning_effort
req_verbosity debug_session_id
'''.split())
FIELD_TYPES = {**dict.fromkeys(INTEGER_FIELDS, 'INTEGER'),
               **dict.fromkeys(REAL_FIELDS, 'REAL'),
               **dict.fromkeys(ALIASED_FIELDS, 'TEXT'),
               **dict.fromkeys(EMPTY_FIELDS, 'TEXT'),
               **dict.fromkeys(('id', 'key_hash', 'attempts', 'tool_names'), 'TEXT'),
               'debug': 'INTEGER'}
# These code-owned retry classes suppress non-upstream error cards. Replacing
# them would change aggregate.errorEntries semantics for statuses below 500.
ERROR_CLASSES = frozenset(('transport', 'rate_limit'))
ATTEMPT_FIELDS = frozenset(('status_code', 'error_type', 'error_code', 'error_msg', 'retry_after_ms', 'at'))
MARKER_KEY = 'docs_redaction'
POLICY_VERSION = 1
ALIAS_NUMBER = r'[1-9][0-9]*'
HASH = re.compile(r'^[0-9a-f]{64}$')
STAMP = re.compile(r'^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,9})?(?:Z|[+-]\d{2}:\d{2})$')
SUMMARY_SUMS = ('input_tokens', 'output_tokens', 'total_tokens', 'cache_read_tokens',
                'cache_write_tokens', 'reasoning_tokens', 'tool_calls', 'cost')
# Store.Open may add these canonical runtime tables to a derived copy. Their
# data policy is stricter than ordinary storage: no captures or operator state.
AUXILIARY_TYPES = {
    'meta': {'key': 'TEXT', 'value': 'TEXT'},
    'request_debug': {'id': 'TEXT', 'session_id': 'TEXT', 'captured_at': 'INTEGER',
                      'expires_at': 'INTEGER', 'payload': 'TEXT'},
    'request_projection_state': dict.fromkeys(('singleton', 'epoch', 'high_rowid'), 'INTEGER'),
}
INDEX_TABLES = {
    **dict.fromkeys(('idx_requests_log', 'idx_requests_provider', 'idx_requests_model',
                    'sqlite_autoindex_requests_1'), 'requests'),
    **dict.fromkeys(('idx_request_debug_expires', 'idx_request_debug_session',
                    'sqlite_autoindex_request_debug_1'), 'request_debug'),
    'sqlite_autoindex_meta_1': 'meta',
}
TRIGGER_NAMES = frozenset('requests_projection_' + suffix for suffix in ('replace', 'insert', 'update', 'delete'))


def require(condition, message):
    if not condition:
        # Messages identify the policy violation, never source values.
        raise ValueError('documentation copy: ' + message)


def _new_file(path):
    fd = os.open(path, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
    os.close(fd)


def _readonly(path):
    def strict_text(data):
        try:
            return data.decode('utf-8')
        except UnicodeDecodeError:
            # SQLite's default decoder error embeds the original cell value.
            raise ValueError('documentation copy: invalid UTF-8 text') from None

    connection = sqlite3.connect(path.resolve(strict=True).as_uri() + '?mode=ro', uri=True)
    connection.text_factory = strict_text
    return connection


def _table_columns(connection, table, types, primary_key):
    # table is an internal policy name, never source-provided SQL. xinfo also
    # includes generated/hidden columns, which table_info would silently omit.
    info = connection.execute('PRAGMA table_xinfo(' + table + ')').fetchall()
    columns = [row[1] for row in info]
    require(len(columns) == len(types) and set(columns) == set(types), 'unrecognized table columns')
    require(all(row[2].upper() == types[row[1]] and row[6] == 0 for row in info),
            'unrecognized column types or generated data')
    require([row[1] for row in info if row[5]] == [primary_key], 'unexpected table primary key')
    return columns


def _columns(connection):
    require(connection.execute("SELECT type FROM sqlite_schema WHERE name='requests'").fetchone() == ('table',),
            'request data must be a table')
    return _table_columns(connection, 'requests', FIELD_TYPES, 'id')


def _json(value):
    require(type(value) is str, 'non-text JSON field')

    def unique_fields(pairs):
        result = {}
        for key, value in pairs:
            require(key not in result, 'duplicate JSON field')
            result[key] = value
        return result

    try:
        # Structured fields have a defined UTF-8 encoding. Surrogate escapes
        # are permitted only while identifying source labels, never in JSON.
        value.encode('utf-8')
        return json.loads(value, object_pairs_hook=unique_fields,
                          parse_constant=lambda _: require(False, 'non-finite JSON number'))
    except UnicodeEncodeError:
        raise ValueError('documentation copy: invalid UTF-8 JSON field') from None
    except (TypeError, json.JSONDecodeError):
        raise ValueError('documentation copy: malformed JSON field') from None


def _encode(value):
    return json.dumps(value, separators=(',', ':'), ensure_ascii=True, allow_nan=False)


class _Aliases:
    def __init__(self):
        self.names = {}
        self.key_salt = secrets.token_bytes(32)

    def __call__(self, kind, value):
        require(type(value) is str, 'non-text identifier')
        if not value or kind == 'error type' and value in ERROR_CLASSES:
            return value
        if kind == 'key':
            return hmac.new(self.key_salt, value.encode('utf-8', errors='surrogateescape'), hashlib.sha256).hexdigest()
        names = self.names.setdefault(kind, {})
        if value not in names:
            names[value] = kind + ' ' + str(len(names) + 1)
        return names[value]


def _alias_valid(kind, value):
    return (value == '' or kind == 'error type' and value in ERROR_CLASSES
            or re.fullmatch(re.escape(kind) + ' ' + ALIAS_NUMBER, value) is not None)


def _attempts(value, aliases=None):
    attempts = _json(value) if value else None  # Historical empty encoding means no attempts.
    require(attempts is None or type(attempts) is list, 'attempts must be an array or null')
    if attempts is None:
        return None
    result = []
    for attempt in attempts:
        require(type(attempt) is dict and set(attempt) <= ATTEMPT_FIELDS,
                'unrecognized attempt fields')
        require(type(attempt.get('status_code')) is int and type(attempt.get('at')) is str,
                'attempt status and timestamp are required')
        require(STAMP.fullmatch(attempt['at']) is not None, 'invalid attempt timestamp')
        try:
            datetime.datetime.fromisoformat(attempt['at'])
        except ValueError:
            raise ValueError('documentation copy: invalid attempt timestamp') from None
        out = dict(attempt)
        for key, value in attempt.items():
            if key in ('status_code', 'retry_after_ms'):
                require(type(value) is int and -(1 << 63) <= value < (1 << 63), 'non-integer attempt metric')
            elif key != 'at':
                require(type(value) is str, 'non-text attempt detail')
                kind = ALIASED_FIELDS[key]
                if aliases is not None:
                    out[key] = aliases(kind, value)
                else:
                    require(_alias_valid(kind, value), 'unredacted attempt detail')
        result.append(out)
    return result


def _record(columns, values, number, aliases=None):
    record = dict(zip(columns, values, strict=True))
    for name, value in record.items():
        if FIELD_TYPES[name] == 'INTEGER':
            require(type(value) is int and -(1 << 63) <= value < (1 << 63), 'non-integer request metric')
        elif FIELD_TYPES[name] == 'REAL':
            require(type(value) in (int, float) and math.isfinite(value), 'non-finite request metric')
        else:
            require(type(value) is str, 'non-text request field')
    require(record['id'] != '', 'missing request identity')
    for name, kind in ALIASED_FIELDS.items():
        if aliases is not None:
            record[name] = aliases(kind, record[name])
        else:
            require(_alias_valid(kind, record[name]), 'unredacted request identifier or detail')
    if aliases is not None:
        record['id'] = 'request ' + str(number)
        record['key_hash'] = aliases('key', record['key_hash'])
    else:
        require(re.fullmatch('request ' + ALIAS_NUMBER, record['id']) is not None,
                'unredacted request identity')
        require(not record['key_hash'] or HASH.fullmatch(record['key_hash']) is not None,
                'invalid pseudonymous key hash')
    require(all(record[name] == '' for name in EMPTY_FIELDS) and record['debug'] == 0,
            'unredacted request content or debug state')
    record['attempts'] = _encode(_attempts(record['attempts'], aliases))
    names = _json(record['tool_names']) if record['tool_names'] else None
    require(names is None or type(names) is list and all(type(name) is str for name in names),
            'invalid tool-name array')
    if names is not None:
        if aliases is not None:
            names = [aliases('tool', name) for name in names]
        else:
            require(all(_alias_valid('tool', name) for name in names), 'unredacted tool name')
    record['tool_names'] = _encode(names)
    return [record[name] for name in columns], record


class _Manifest:
    def __init__(self, columns):
        self.digest = hashlib.sha256((_encode(columns) + '\n').encode())
        self.summary = dict.fromkeys(SUMMARY_SUMS, 0)
        self.summary.update(records=0, min_started_ms=None, max_started_ms=None, parent_declarations=0)

    def add(self, values, record):
        self.digest.update((_encode(values) + '\n').encode())
        summary = self.summary
        summary['records'] += 1
        summary['parent_declarations'] += bool(record['parent_conversation_id'])
        start = record['started_at']
        summary['min_started_ms'] = start if summary['min_started_ms'] is None else min(start, summary['min_started_ms'])
        summary['max_started_ms'] = start if summary['max_started_ms'] is None else max(start, summary['max_started_ms'])
        for name in SUMMARY_SUMS:
            summary[name] += record[name]
        require(math.isfinite(summary['cost']), 'non-finite total cost')

    def marker(self):
        return {'policy': POLICY_VERSION, 'sha256': self.digest.hexdigest(), 'summary': self.summary}


def _derive(snapshot, destination):
    with closing(_readonly(snapshot)) as src:
        columns = _columns(src)
        # Discard content in SQL, before SQLite/Python can decode or allocate
        # it. Source labels may contain arbitrary bytes: surrogateescape keeps
        # their identity reversible until replaced with safe aliases. No such
        # source string is ever inserted into the destination.
        projection = ','.join("CAST('' AS TEXT)" if name in EMPTY_FIELDS else '0' if name == 'debug'
                              else '"' + name + '"' for name in columns)
        src.text_factory = lambda data: data.decode('utf-8', errors='surrogateescape')
        aliases, manifest = _Aliases(), _Manifest(columns)
        _new_file(destination)
        try:
            with closing(sqlite3.connect(destination)) as dst:
                # Only validated column identifiers/types cross this boundary.
                # No source schema SQL, triggers, defaults, metadata, sidecars or
                # old/free SQLite pages are copied to the served destination.
                definitions = ','.join('"' + name + '" ' + FIELD_TYPES[name]
                                       + (' PRIMARY KEY' if name == 'id' else ' NOT NULL') for name in columns)
                dst.execute('CREATE TABLE requests (' + definitions + ')')
                dst.execute('CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)')
                placeholders = ','.join('?' for _ in columns)
                with dst:
                    for number, row in enumerate(src.execute('SELECT ' + projection + ' FROM requests ORDER BY rowid'), 1):
                        values, record = _record(columns, row, number, aliases)
                        dst.execute('INSERT INTO requests VALUES (' + placeholders + ')', values)
                        manifest.add(values, record)
                    dst.execute('INSERT INTO meta VALUES (?, ?)', (MARKER_KEY, _encode(manifest.marker())))
            verify_docs_copy(destination)
        except BaseException:
            destination.unlink(missing_ok=True)
            raise


def verify_docs_copy(path):
    """Revalidate the entire derived database, returning only numerical facts.

    Call before serving/capturing, and again after capture. A marker alone is
    insufficient: every row, JSON field, table and digest must still conform.
    """
    path = Path(path)
    require(not path.is_symlink(), 'symlinked documentation database')
    with closing(_readonly(path)) as connection:
        connection.execute('BEGIN')
        columns = _columns(connection)
        objects = connection.execute('SELECT type, name, tbl_name FROM sqlite_schema').fetchall()
        tables = {name for kind, name, _ in objects if kind == 'table'}
        require({'requests', 'meta'} <= tables <= {'requests', *AUXILIARY_TYPES},
                'unrecognized documentation tables')
        for kind, name, table in objects:
            require(kind == 'table' or kind == 'index' and INDEX_TABLES.get(name) == table
                    or kind == 'trigger' and name in TRIGGER_NAMES and table == 'requests',
                    'unrecognized documentation schema object')
        for table in tables - {'requests'}:
            types = AUXILIARY_TYPES[table]
            _table_columns(connection, table, types, next(iter(types)))
        if 'request_debug' in tables:
            require(connection.execute('SELECT COUNT(*) FROM request_debug').fetchone()[0] == 0,
                    'documentation copy contains debug captures')
        if 'request_projection_state' in tables:
            state = connection.execute('SELECT singleton, epoch, high_rowid FROM request_projection_state').fetchall()
            require(len(state) == 1 and state[0][0] == 1
                    and all(type(value) is int and 0 <= value < (1 << 63) for value in state[0]),
                    'unexpected projection state')
        metadata = dict(connection.execute('SELECT key, value FROM meta'))
        require(set(metadata) <= {MARKER_KEY, 'attempts_repaired'} and MARKER_KEY in metadata,
                'missing provenance or unexpected operator metadata')
        require(metadata.get('attempts_repaired', '1') == '1', 'unexpected migration metadata')
        marker = _json(metadata[MARKER_KEY])
        require(type(marker) is dict and set(marker) == {'policy', 'sha256', 'summary'}
                and type(marker['policy']) is int and marker['policy'] == POLICY_VERSION,
                'unrecognized documentation provenance')
        manifest = _Manifest(columns)
        for number, row in enumerate(connection.execute('SELECT * FROM requests ORDER BY rowid'), 1):
            values, record = _record(columns, row, number)
            manifest.add(values, record)
        require(_encode(marker) == _encode(manifest.marker()), 'documentation data changed after redaction')
        return dict(manifest.summary)


def backup(source: Path, destination: Path, *, docs_safe=False) -> None:
    source, destination = Path(source), Path(destination)
    if docs_safe:
        # The raw online snapshot is never the served file. TemporaryDirectory
        # provides a private 0700 namespace and removes its files on every exit.
        require(not destination.exists() and not destination.is_symlink(), 'destination already exists')
        with tempfile.TemporaryDirectory(prefix='millivolt-docs-backup-', dir=destination.parent) as temp:
            snapshot = Path(temp) / 'snapshot.db'
            backup(source, snapshot)
            _derive(snapshot, destination)
        return
    source = source.resolve(strict=True)
    with closing(_readonly(source)) as src:
        _new_file(destination)
        try:
            with closing(sqlite3.connect(destination)) as dst:
                src.backup(dst, pages=BACKUP_PAGES)
        except BaseException:
            # Only this newly created, incomplete backup belongs to us.
            destination.unlink(missing_ok=True)
            raise


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("source", type=Path)
    parser.add_argument("destination", type=Path)
    parser.add_argument('--docs-safe', action='store_true', help='derive a verified metrics-only copy with pseudonymous identifiers for documentation')
    args = parser.parse_args()
    backup(args.source, args.destination, docs_safe=args.docs_safe)


if __name__ == "__main__":
    main()
