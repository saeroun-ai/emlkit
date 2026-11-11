#!/usr/bin/env python3
"""Dump .eml files as Python's email package parses them, in the emlkit
differential schema (docs/robustness-notes/differential.md).

One JSON object per input file is written to standard output, one per line,
in the order the files were given. The script uses the standard library only,
reads nothing but the named files and writes nothing but standard output.
Run it isolated from the environment:

    timeout 60 python3 -I scripts/diff/py_email_dump.py testdata/corpus/*.eml
"""

import argparse
import datetime
import email
import email.header
import email.message
import email.policy
import email.utils
import hashlib
import json
import os
import platform
import re
import sys

SCHEMA = "emlkit-differential/2"

# A raw 8-bit byte the parser could not decode is a surrogate escape, one per
# byte. The schema carries strings as UTF-8, so each becomes U+FFFD, as the
# emlkit driver writes each byte that is not valid UTF-8.
SURROGATE_ESCAPE = re.compile("[\udc80-\udcff]")

# Bounds on what one input may cost: its size, the depth of the part tree
# walked, and the number of parts recorded. Past them the record says so
# instead of growing.
DEFAULT_MAX_BYTES = 32 << 20
MAX_DEPTH = 64
MAX_PARTS = 10000


def lower_or_none(value):
    if value is None:
        return None
    value = str(value).strip().lower()
    return value or None


def header_text(msg, name, compat32):
    """The decoded text of the first field called name, or None."""
    value = msg.get(name)
    if value is None:
        return None
    if compat32:
        try:
            return str(email.header.make_header(email.header.decode_header(value)))
        except Exception:
            return str(value)
    return str(value)


def addresses(msg, name, compat32):
    """The addresses of the first field called name, or None without one."""
    value = msg.get(name)
    if value is None:
        return None
    out = []
    try:
        if compat32:
            text = header_text(msg, name, True)
            for display, spec in email.utils.getaddresses([text]):
                if display or spec:
                    out.append({"name": display, "address": spec})
        else:
            for addr in value.addresses:
                out.append({"name": addr.display_name, "address": addr.addr_spec})
    except Exception:
        # A field the parser could not read as addresses: none of them.
        return []
    return out


def date_utc(msg, compat32):
    """The Date field as a UTC instant, RFC 3339 with a Z, or None."""
    value = msg.get("date")
    if value is None:
        return None
    try:
        if compat32:
            dt = email.utils.parsedate_to_datetime(str(value))
        else:
            dt = value.datetime
    except Exception:
        return None
    if dt is None:
        return None
    if dt.tzinfo is None:
        # "-0000": the time is UTC with no statement about the local zone.
        dt = dt.replace(tzinfo=datetime.timezone.utc)
    return dt.astimezone(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def message_id(msg):
    """The identifier of the first Message-ID field: what its first pair of
    angle brackets encloses, white space removed, or the trimmed value when it
    has none; None without the field or an identifier."""
    value = msg.get("message-id")
    if value is None:
        return None
    text = str(value)
    lt = text.find("<")
    gt = text.find(">", lt + 1) if lt >= 0 else -1
    if gt > lt >= 0:
        text = "".join(text[lt + 1:gt].split())
    else:
        text = text.strip()
    return text or None


def content_id(part):
    value = part.get("content-id")
    if value is None:
        return None
    value = str(value).strip().strip("<>").strip()
    return value or None


def clean(value):
    """value with every string in it as the schema carries strings."""
    if isinstance(value, str):
        return SURROGATE_ESCAPE.sub("�", value)
    if isinstance(value, list):
        return [clean(v) for v in value]
    if isinstance(value, dict):
        return {k: clean(v) for k, v in value.items()}
    return value


def text_sha256(part, data, compat32):
    """The digest of the text as UTF-8, or None when it cannot be decoded."""
    if part.get_content_maintype() != "text" or data is None:
        return None
    if not compat32:
        try:
            text = part.get_content()
        except Exception:
            return None
        if isinstance(text, bytes):
            return None
    else:
        charset = part.get_content_charset() or "us-ascii"
        try:
            text = data.decode(charset, errors="replace")
        except (LookupError, ValueError):
            return None
    return hashlib.sha256(text.encode("utf-8", errors="surrogateescape")).hexdigest()


class Walker:
    def __init__(self, compat32):
        self.compat32 = compat32
        self.parts = 0

    def part(self, part, path):
        self.parts += 1
        rec = {
            "path": path,
            "media_type": part.get_content_type(),
            "charset": part.get_content_charset(),
            "disposition": part.get_content_disposition(),
            "filename": None,
            "content_id": content_id(part),
            "transfer_encoding": lower_or_none(part.get("content-transfer-encoding")),
            "body_length": None,
            "body_sha256": None,
            "text_sha256": None,
            "children": None,
            "defects": [],
        }
        try:
            rec["filename"] = part.get_filename()
        except Exception as exc:
            rec["filename_error"] = type(exc).__name__
        data = None
        if part.is_multipart():
            payload = part.get_payload()
            if not isinstance(payload, list):
                payload = [payload]
            children = []
            for i, child in enumerate(payload):
                if len(path) >= MAX_DEPTH or self.parts >= MAX_PARTS:
                    rec["truncated"] = True
                    break
                if isinstance(child, email.message.Message):
                    children.append(self.part(child, path + [i]))
            rec["children"] = children
        else:
            try:
                data = part.get_payload(decode=True)
            except Exception as exc:
                data = None
                rec["body_error"] = type(exc).__name__
            if isinstance(data, str):
                data = data.encode("utf-8", errors="surrogateescape")
        # Decoding the body adds its defects, so they are read after it, and
        # before the text is decoded, which would decode the body again and
        # add them a second time.
        rec["defects"] = [type(d).__name__ for d in getattr(part, "defects", [])]
        if not part.is_multipart() and data is not None:
            rec["body_length"] = len(data)
            rec["body_sha256"] = hashlib.sha256(data).hexdigest()
            rec["text_sha256"] = text_sha256(part, data, self.compat32)
        return rec


def dump(path, policy, policy_name, max_bytes):
    fixture = os.path.basename(path)
    if fixture.endswith(".eml"):
        fixture = fixture[: -len(".eml")]
    # The parser name is what exceptions are matched by, so each policy has
    # its own: a difference reviewed under one is not explained under the other.
    out = {
        "schema": SCHEMA,
        "parser": "python-email" if policy_name == "default" else "python-email-" + policy_name,
        "parser_version": platform.python_version(),
        "policy": policy_name,
        "fixture": fixture,
        "file": path,
    }
    try:
        with open(path, "rb") as f:
            data = f.read(max_bytes + 1)
    except OSError as exc:
        out["error"] = "read: " + type(exc).__name__
        return out
    if len(data) > max_bytes:
        out["error"] = "input larger than %d bytes" % max_bytes
        return out
    compat32 = policy_name == "compat32"
    try:
        msg = email.message_from_bytes(data, policy=policy)
        walker = Walker(compat32)
        root = walker.part(msg, [])
        out["subject"] = header_text(msg, "subject", compat32)
        out["from"] = addresses(msg, "from", compat32)
        out["to"] = addresses(msg, "to", compat32)
        out["cc"] = addresses(msg, "cc", compat32)
        out["reply_to"] = addresses(msg, "reply-to", compat32)
        out["message_id"] = message_id(msg)
        out["date"] = date_utc(msg, compat32)
        out["root"] = root
    except RecursionError:
        out["error"] = "parse: RecursionError"
    except Exception as exc:
        out["error"] = "parse: " + type(exc).__name__
    return clean(out)


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("files", nargs="+", help=".eml files to dump")
    ap.add_argument("--compat32", action="store_true",
                    help="parse with email.policy.compat32 instead of email.policy.default")
    ap.add_argument("--max-bytes", type=int, default=DEFAULT_MAX_BYTES,
                    help="refuse an input larger than this (default %(default)s)")
    args = ap.parse_args()
    if args.compat32:
        policy, policy_name = email.policy.compat32, "compat32"
    else:
        policy, policy_name = email.policy.default, "default"
    for path in args.files:
        rec = dump(path, policy, policy_name, args.max_bytes)
        sys.stdout.write(json.dumps(rec, sort_keys=True) + "\n")
        sys.stdout.flush()
    return 0


if __name__ == "__main__":
    sys.exit(main())
