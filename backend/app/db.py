"""Persistance SQLite : sessions et messages.

On utilise sqlite3 de la bibliothèque standard (zéro dépendance). Les écritures
sont rapides ; un verrou protège l'accès concurrent depuis les routes async.
"""
from __future__ import annotations

import json
import os
import sqlite3
import threading
import time
import uuid

from .config import settings

_LOCK = threading.Lock()
_DB_PATH = os.path.join(settings.data_dir, "loki.db")


def _connect() -> sqlite3.Connection:
    os.makedirs(settings.data_dir, exist_ok=True)
    conn = sqlite3.connect(_DB_PATH, check_same_thread=False)
    conn.row_factory = sqlite3.Row
    return conn


def init_db() -> None:
    """Crée les tables si elles n'existent pas."""
    with _LOCK, _connect() as conn:
        conn.executescript(
            """
            CREATE TABLE IF NOT EXISTS sessions (
                id          TEXT PRIMARY KEY,
                title       TEXT NOT NULL,
                model       TEXT,
                created_at  REAL NOT NULL,
                updated_at  REAL NOT NULL
            );
            CREATE TABLE IF NOT EXISTS messages (
                id          TEXT PRIMARY KEY,
                session_id  TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
                role        TEXT NOT NULL,
                content     TEXT NOT NULL,
                model       TEXT,
                meta        TEXT,
                created_at  REAL NOT NULL
            );
            CREATE INDEX IF NOT EXISTS idx_messages_session
                ON messages(session_id, created_at);
            """
        )
        # Migration douce : ajoute la colonne meta aux bases antérieures.
        cols = {r["name"] for r in conn.execute("PRAGMA table_info(messages)")}
        if "meta" not in cols:
            conn.execute("ALTER TABLE messages ADD COLUMN meta TEXT")
        # Table clé/valeur pour la configuration de l'agent.
        conn.execute(
            "CREATE TABLE IF NOT EXISTS config (key TEXT PRIMARY KEY, value TEXT)"
        )


def _now() -> float:
    return time.time()


# ── Sessions ─────────────────────────────────────────────────────────────
def create_session(title: str, model: str | None) -> dict:
    sid = uuid.uuid4().hex
    now = _now()
    with _LOCK, _connect() as conn:
        conn.execute(
            "INSERT INTO sessions (id, title, model, created_at, updated_at)"
            " VALUES (?, ?, ?, ?, ?)",
            (sid, title, model, now, now),
        )
    return {"id": sid, "title": title, "model": model,
            "created_at": now, "updated_at": now, "message_count": 0}


def list_sessions() -> list[dict]:
    with _LOCK, _connect() as conn:
        rows = conn.execute(
            """
            SELECT s.*, COUNT(m.id) AS message_count
            FROM sessions s
            LEFT JOIN messages m ON m.session_id = s.id
            GROUP BY s.id
            ORDER BY s.updated_at DESC
            """
        ).fetchall()
    return [dict(r) for r in rows]


def get_session(sid: str) -> dict | None:
    with _LOCK, _connect() as conn:
        row = conn.execute("SELECT * FROM sessions WHERE id = ?", (sid,)).fetchone()
    return dict(row) if row else None


def rename_session(sid: str, title: str) -> None:
    with _LOCK, _connect() as conn:
        conn.execute(
            "UPDATE sessions SET title = ?, updated_at = ? WHERE id = ?",
            (title, _now(), sid),
        )


def delete_session(sid: str) -> None:
    with _LOCK, _connect() as conn:
        conn.execute("DELETE FROM messages WHERE session_id = ?", (sid,))
        conn.execute("DELETE FROM sessions WHERE id = ?", (sid,))


def touch_session(sid: str) -> None:
    with _LOCK, _connect() as conn:
        conn.execute(
            "UPDATE sessions SET updated_at = ? WHERE id = ?", (_now(), sid)
        )


# ── Messages ─────────────────────────────────────────────────────────────
def add_message(
    sid: str,
    role: str,
    content: str,
    model: str | None,
    meta: dict | None = None,
) -> dict:
    mid = uuid.uuid4().hex
    now = _now()
    meta_json = json.dumps(meta, ensure_ascii=False) if meta else None
    with _LOCK, _connect() as conn:
        conn.execute(
            "INSERT INTO messages (id, session_id, role, content, model, meta, created_at)"
            " VALUES (?, ?, ?, ?, ?, ?, ?)",
            (mid, sid, role, content, model, meta_json, now),
        )
        conn.execute(
            "UPDATE sessions SET updated_at = ? WHERE id = ?", (now, sid)
        )
    return {"id": mid, "session_id": sid, "role": role, "content": content,
            "model": model, "meta": meta, "created_at": now}


def _row_to_message(row: sqlite3.Row) -> dict:
    msg = dict(row)
    msg["meta"] = json.loads(msg["meta"]) if msg.get("meta") else None
    return msg


def list_messages(sid: str) -> list[dict]:
    with _LOCK, _connect() as conn:
        rows = conn.execute(
            "SELECT * FROM messages WHERE session_id = ? ORDER BY created_at",
            (sid,),
        ).fetchall()
    return [_row_to_message(r) for r in rows]


def list_messages_for_model(sid: str) -> list[dict]:
    """Historique épuré (role/content) destiné au contexte du modèle."""
    return [
        {"role": m["role"], "content": m["content"]}
        for m in list_messages(sid)
    ]


# ── Configuration (clé/valeur JSON) ──────────────────────────────────────
def get_config_value(key: str) -> dict | None:
    with _LOCK, _connect() as conn:
        row = conn.execute(
            "SELECT value FROM config WHERE key = ?", (key,)
        ).fetchone()
    return json.loads(row["value"]) if row else None


def set_config_value(key: str, value: dict) -> None:
    payload = json.dumps(value, ensure_ascii=False)
    with _LOCK, _connect() as conn:
        conn.execute(
            "INSERT INTO config (key, value) VALUES (?, ?)"
            " ON CONFLICT(key) DO UPDATE SET value = excluded.value",
            (key, payload),
        )
