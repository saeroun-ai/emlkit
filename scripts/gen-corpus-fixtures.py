#!/usr/bin/env python3
"""Build the sender-shaped fixtures of the regression corpus (ROB-130-003).

Every fixture is hand-built and synthetic: invented names, example.* domains,
documentation address ranges, placeholder attachment bytes. The bytes are
built here rather than typed so that legacy-charset text, CR-only line
endings, NUL bytes and cut-off endings are exact, and so that anyone can
reproduce them.

Usage, from the repository root (stdlib only, deterministic):

    timeout 60 python3 -I scripts/gen-corpus-fixtures.py          # write
    timeout 60 python3 -I scripts/gen-corpus-fixtures.py --check  # compare

Writing touches only the .eml files listed in FIXTURES and, in
testdata/corpus/manifest.json, the name, category and description of those
same fixtures (an entry is added when missing; every other entry and every
recorded outcome is left alone). The outcomes are then recorded by the Go
test:

    go test -run TestCorpus .                 # with EMLKIT_UPDATE_CORPUS=1
    go test -run TestCorpusWithPlugin ./charset/

--check writes nothing and exits 1 when a listed file differs from what this
script would write. See docs/robustness-notes/corpus.md.
"""

import base64
import json
import os
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
CORPUS = os.path.join(ROOT, "testdata", "corpus")
MANIFEST = os.path.join(CORPUS, "manifest.json")
CATEGORIES = ("conformant", "malformed", "truncated", "adversarial", "provider-style")

CRLF = b"\r\n"
DATE = "Wed, 11 Nov 2026 09:00:00 +0900"

FIXTURES = []


def fixture(name, category, description):
    assert category in CATEGORIES, category

    def register(build):
        FIXTURES.append((name, category, description, build))
        return build

    return register


# --- helpers ---------------------------------------------------------------


def raw(s):
    """ASCII text as bytes; anything else must be encoded explicitly."""
    if isinstance(s, bytes):
        return s
    return s.encode("ascii")


def flat(items):
    for item in items:
        if isinstance(item, (list, tuple)):
            yield from flat(item)
        else:
            yield raw(item)


def msg(*items, eol=CRLF):
    """Every line terminated by eol."""
    return b"".join(line + eol for line in flat(items))


def b64(data, width=76):
    s = base64.b64encode(data)
    return [s[i:i + width] for i in range(0, len(s), width)]


def qp(data, width=76):
    """Quoted-printable lines of data, whose line breaks are LF or CRLF."""
    out = []
    for line in data.split(b"\n"):
        line = line[:-1] if line.endswith(b"\r") else line
        toks = []
        for i, c in enumerate(line):
            last = i == len(line) - 1
            if (33 <= c <= 126 and c != 61) or (c in (9, 32) and not last):
                toks.append(bytes([c]))
            else:
                toks.append(b"=%02X" % c)
        cur = b""
        for tok in toks:
            if len(cur) + len(tok) > width - 1:
                out.append(cur + b"=")
                cur = b""
            cur += tok
        out.append(cur)
    if out and out[-1] == b"" and data.endswith(b"\n"):
        out.pop()
    return out


def ew_b(charset, data):
    return b"=?" + raw(charset) + b"?B?" + base64.b64encode(data) + b"?="


def ew_q(charset, data):
    enc = b""
    for c in data:
        if c == 32:
            enc += b"_"
        elif 33 <= c <= 126 and c not in b"=?_":
            enc += bytes([c])
        else:
            enc += b"=%02X" % c
    return b"=?" + raw(charset) + b"?Q?" + enc + b"?="


def pct(data):
    """RFC 2231 percent encoding: attribute-chars stay, the rest is %XX."""
    keep = b"abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789!#$&+-.^_`|~"
    return b"".join(bytes([c]) if c in keep else b"%%%02X" % c for c in data)


def crlf(text):
    """Text with LF line breaks, as CRLF."""
    return text.replace("\n", "\r\n")


def ksx1001(text):
    """EUC-KR bytes of text that is entirely within KS X 1001.

    Python's euc_kr codec writes a syllable outside KS X 1001 as an
    eight-byte jamo composition, which is not what senders produce; such
    text must be built with cp949 on purpose instead.
    """
    data = text.encode("euc_kr")
    assert data == text.encode("cp949"), text
    return data


def cp949_extension(text):
    """CP949 bytes of text, which must hold syllables outside KS X 1001."""
    data = text.encode("cp949")
    i, ext = 0, 0
    while i < len(data):
        if data[i] < 0x80:
            i += 1
            continue
        if data[i] < 0xA1 or data[i + 1] < 0xA1:
            ext += 1
        i += 2
    assert ext > 0, text
    return data


def iso2022jp(text):
    """ISO-2022-JP, each line encoded on its own so it ends in ASCII."""
    lines = [line.encode("iso2022_jp") for line in text.split("\n")]
    return b"\r\n".join(lines)


# Placeholder attachment bodies: a recognisable magic number, then a label.
PDF = b"%PDF-1.4\n% placeholder, not a document\n%%EOF\n"
PNG = b"\x89PNG\r\n\x1a\n" + b"placeholder image bytes, not a picture" + bytes(8)
JPEG = b"\xff\xd8\xff\xe0" + b"placeholder image bytes, not a picture" + b"\xff\xd9"
ZIP = b"PK\x03\x04" + b"placeholder archive bytes, not an archive" + bytes(16)
XLS = b"\xd0\xcf\x11\xe0\xa1\xb1\x1a\xe1" + b"placeholder workbook bytes" + bytes(8)
TNEF = b"\x78\x9f\x3e\x22\x01\x00" + b"placeholder TNEF stream, not a real winmail.dat" + bytes(8)
P7S = b"\x30\x82\x01\x00\x06\x09" + b"placeholder signature bytes, not a CMS structure" + bytes(8)


# --- webmail and client shapes ---------------------------------------------


@fixture("provider-gmail-alternative", "provider-style",
         "Gmail-style multipart/alternative: base64 UTF-8 text/plain and quoted-printable UTF-8 text/html, "
         "Korean Subject and From as UTF-8 B words.")
def gmail():
    plain = crlf("안녕하세요,\n\n이번 주 주간 보고서를 공유드립니다. 아래 링크에서 확인해 주세요.\n"
                 "https://docs.example.com/d/weekly-report\n\n감사합니다.\n홍길동\n").encode()
    html = ('<div dir="ltr">안녕하세요,<div><br></div><div>이번 주 주간 보고서를 공유드립니다. '
            '아래 링크에서 확인해 주세요.</div><div><a href="https://docs.example.com/d/weekly-report">'
            'https://docs.example.com/d/weekly-report</a></div><div><br></div><div>감사합니다.</div>'
            '<div>홍길동</div></div>\n').encode()
    bd = "000000000000a1b2c3d4e5f60718"
    return msg(
        "MIME-Version: 1.0",
        "Received: by 2001:db8::1 with SMTP id a0b1c2d3e4f5;",
        "        " + DATE,
        "Date: " + DATE,
        "Message-ID: <CAExample0000000000000000000000001@mail.example.com>",
        b"Subject: " + ew_b("UTF-8", "주간 보고서 공유드립니다".encode()),
        b"From: " + ew_b("UTF-8", "홍길동".encode()) + b" <gildong.hong@example.com>",
        "To: Test Recipient <recipient@example.org>",
        'Content-Type: multipart/alternative; boundary="%s"' % bd,
        "",
        "--" + bd,
        'Content-Type: text/plain; charset="UTF-8"',
        "Content-Transfer-Encoding: base64",
        "",
        b64(plain),
        "",
        "--" + bd,
        'Content-Type: text/html; charset="UTF-8"',
        "Content-Transfer-Encoding: quoted-printable",
        "",
        qp(html),
        "",
        "--" + bd + "--",
    )


@fixture("provider-outlook-exchange-tnef", "provider-style",
         "Outlook/Exchange-style mixed > alternative > related, with Thread-Index, X-MS-* fields, an inline "
         "image by Content-ID and a winmail.dat application/ms-tnef part holding placeholder bytes.")
def outlook():
    plain = b"Hello team,\r\n\r\nPlease find the quarterly figures attached.\r\n\r\nRegards,\r\nTest Sender\r\n"
    html = (b'<html xmlns:v="urn:schemas-microsoft-com:vml" xmlns:o="urn:schemas-microsoft-com:office:office">'
            b'<head><meta http-equiv="Content-Type" content="text/html; charset=us-ascii"></head>'
            b'<body lang="EN-US"><div class="WordSection1"><p class="MsoNormal">Hello team,<o:p></o:p></p>'
            b'<p class="MsoNormal">Please find the quarterly figures attached.<o:p></o:p></p>'
            b'<p class="MsoNormal"><img width="16" height="16" id="Picture_x0020_1" '
            b'src="cid:image001.png@01DB0000.00000000"></p></div></body></html>\r\n')
    mixed, alt, rel = "_004_EX0PR00MB0000EXAMPLE_", "_000_EX0PR00MB0000EXAMPLE_", "_005_EX0PR00MB0000EXAMPLE_"
    stamp = 'creation-date="Wed, 11 Nov 2026 00:00:00 GMT";'
    return msg(
        "Received: from EX0PR00MB0000.example.com (2001:db8::10) by",
        " EX0PR00MB0001.example.com (2001:db8::11) with Microsoft SMTP Server",
        " id 15.20.0000.000; " + DATE,
        "From: Test Sender <sender@corp.example.com>",
        "To: Test Team <team@corp.example.com>",
        "Subject: Quarterly figures",
        "Thread-Topic: Quarterly figures",
        "Thread-Index: AdtEeHAmcGxhY2Vob2xkZXIgdGhyZWFkIGluZGV4AA==",
        "Date: Wed, 11 Nov 2026 00:00:00 +0000",
        "Message-ID: <EX0PR00MB0000EXAMPLE0000000000@EX0PR00MB0000.example.com>",
        "Accept-Language: ko-KR, en-US",
        "Content-Language: en-US",
        "X-MS-Has-Attach: yes",
        "X-MS-Exchange-Organization-SCL: -1",
        "X-MS-TNEF-Correlator: <EX0PR00MB0000EXAMPLE0000000000@EX0PR00MB0000.example.com>",
        "X-MS-Exchange-Organization-AuthAs: Internal",
        "X-MS-Office365-Filtering-Correlation-Id: 00000000-0000-0000-0000-000000000000",
        "Content-Type: multipart/mixed;",
        '\tboundary="%s"' % mixed,
        "MIME-Version: 1.0",
        "",
        "--" + mixed,
        "Content-Type: multipart/alternative;",
        '\tboundary="%s"' % alt,
        "",
        "--" + alt,
        'Content-Type: text/plain; charset="us-ascii"',
        "Content-Transfer-Encoding: quoted-printable",
        "",
        qp(plain),
        "",
        "--" + alt,
        "Content-Type: multipart/related;",
        '\tboundary="%s";' % rel,
        '\ttype="text/html"',
        "",
        "--" + rel,
        'Content-Type: text/html; charset="us-ascii"',
        "Content-Transfer-Encoding: quoted-printable",
        "",
        qp(html),
        "",
        "--" + rel,
        'Content-Type: image/png; name="image001.png"',
        "Content-Description: image001.png",
        'Content-Disposition: inline; filename="image001.png"; size=%d;' % len(PNG),
        "\t" + stamp,
        '\tmodification-date="Wed, 11 Nov 2026 00:00:00 GMT"',
        "Content-ID: <image001.png@01DB0000.00000000>",
        "Content-Transfer-Encoding: base64",
        "",
        b64(PNG),
        "",
        "--" + rel + "--",
        "",
        "--" + alt + "--",
        "",
        "--" + mixed,
        'Content-Type: application/ms-tnef; name="winmail.dat"',
        "Content-Description: winmail.dat",
        'Content-Disposition: attachment; filename="winmail.dat"; size=%d;' % len(TNEF),
        "\t" + stamp,
        '\tmodification-date="Wed, 11 Nov 2026 00:00:00 GMT"',
        "Content-Transfer-Encoding: base64",
        "",
        b64(TNEF),
        "",
        "--" + mixed + "--",
    )


@fixture("provider-apple-mail-nested-html", "provider-style",
         "Apple Mail-style alternative whose HTML branch is a nested multipart/mixed: HTML, an inline JPEG "
         "with a filename, then more HTML.")
def apple():
    outer = "Apple-Mail=_00000000-0000-4000-8000-000000000001"
    inner = "Apple-Mail=_00000000-0000-4000-8000-000000000002"
    head = '<html><head><meta http-equiv="Content-Type" content="text/html; charset=us-ascii"></head>'
    return msg(
        "From: Test Sender <sender@example.com>",
        "Content-Type: multipart/alternative;",
        '\tboundary="%s"' % outer,
        r"Mime-Version: 1.0 (Mac OS X Mail 16.0 \(3774.0.0.0\))",
        "Subject: Photos from the trip",
        "Date: " + DATE,
        "Message-Id: <00000000-0000-4000-8000-0000000000AA@example.com>",
        "To: Test Recipient <recipient@example.org>",
        "X-Mailer: Apple Mail (2.3774.0.0.0)",
        "",
        "",
        "--" + outer,
        "Content-Transfer-Encoding: 7bit",
        "Content-Type: text/plain;",
        "\tcharset=us-ascii",
        "",
        "Here is the photo.",
        "",
        "",
        "Best,",
        "Sender",
        "",
        "--" + outer,
        "Content-Type: multipart/mixed;",
        '\tboundary="%s"' % inner,
        "",
        "",
        "--" + inner,
        "Content-Transfer-Encoding: 7bit",
        "Content-Type: text/html;",
        "\tcharset=us-ascii",
        "",
        head + '<body style="word-wrap: break-word;">Here is the photo.<br><br></body></html>',
        "--" + inner,
        "Content-Disposition: inline;",
        "\tfilename=photo.jpg",
        "Content-Type: image/jpeg;",
        "\tx-unix-mode=0644;",
        '\tname="photo.jpg"',
        "Content-Transfer-Encoding: base64",
        "",
        b64(JPEG),
        "--" + inner,
        "Content-Transfer-Encoding: 7bit",
        "Content-Type: text/html;",
        "\tcharset=us-ascii",
        "",
        head + '<body style="word-wrap: break-word;"><br><br>Best,<br>Sender</body></html>',
        "--" + inner + "--",
        "",
        "--" + outer + "--",
    )


@fixture("provider-mobile-format-flowed", "provider-style",
         "A mobile client's text/plain; format=flowed; delsp=yes in 8-bit UTF-8: lines broken inside a word "
         "and between words, a quoted flowed paragraph and the '-- ' signature line.")
def mobile():
    body = ("오늘 점심은 12시 반에 회사 앞 식당에서 만나 \n"
            "요. 늦으면 미리 연락  \n"
            "주세요.\n"
            "\n"
            "> 내일 점심 같이 하실래요? 메뉴는 아무거나  \n"
            "> 좋아요.\n"
            "\n"
            "-- \n"
            "Sent from ExampleMail for Android\n")
    return msg(
        "From: =?utf-8?B?" + base64.b64encode("이영희".encode()).decode() + "?= <younghee.lee@example.co.kr>",
        "To: gildong.hong@example.co.kr",
        b"Subject: " + ew_b("utf-8", "Re: 점심 약속".encode()),
        "Date: " + DATE,
        "Message-ID: <mobile-0001@example.co.kr>",
        "In-Reply-To: <desk-0001@example.co.kr>",
        "MIME-Version: 1.0",
        "Content-Type: text/plain; charset=utf-8; format=flowed; delsp=yes",
        "Content-Transfer-Encoding: 8bit",
        "User-Agent: ExampleMail for Android",
        "",
    ) + crlf(body).encode()


# --- Korean mail ----------------------------------------------------------


@fixture("ko-euc-kr-8bit", "conformant",
         "A Korean text/plain body in 8-bit EUC-KR; Subject as an euc-kr B word, From as an EUC-KR Q word.")
def ko_euc_kr():
    body = ("안녕하세요, 홍길동입니다.\n\n11월 정기 점검 일정을 안내드립니다.\n"
            "점검 시간: 11월 14일(토) 02:00 ~ 06:00\n점검 중에는 메일 서비스를 이용하실 수 없습니다.\n\n"
            "감사합니다.\n")
    return msg(
        b"From: " + ew_q("EUC-KR", ksx1001("홍길동")) + b" <gildong.hong@example.co.kr>",
        "To: chulsoo.kim@example.co.kr",
        b"Subject: " + ew_b("euc-kr", ksx1001("[공지] 11월 정기 점검 안내")),
        "Date: " + DATE,
        "Message-ID: <euckr-0001@mail.example.co.kr>",
        "MIME-Version: 1.0",
        "Content-Type: text/plain; charset=euc-kr",
        "Content-Transfer-Encoding: 8bit",
        "",
    ) + ksx1001(crlf(body))


@fixture("ko-ks-c-5601-outlook", "provider-style",
         "Korean Outlook-style alternative labelled ks_c_5601-1987: base64 text/plain and quoted-printable "
         "text/html; Subject as two folded ks_c_5601-1987 Q words, From as a B word.")
def ko_ksc():
    plain = crlf("김철수 님께,\n\n다음 주 정기 회의 안건을 공유드립니다.\n1. 4분기 실적 검토\n"
                 "2. 신규 프로젝트 일정\n\n홍길동 드림\n")
    html = ('<html><head><meta http-equiv="Content-Type" content="text/html; charset=ks_c_5601-1987">'
            '</head><body><p>김철수 님께,</p><p>다음 주 정기 회의 안건을 공유드립니다.</p>'
            '<ol><li>4분기 실적 검토</li><li>신규 프로젝트 일정</li></ol><p>홍길동 드림</p></body></html>\r\n')
    bd = "_000_EX1PR00MB0000EXAMPLEKR_"
    cs = "ks_c_5601-1987"
    return msg(
        b"From: " + ew_b(cs, ksx1001("홍길동")) + b" <gildong.hong@example.co.kr>",
        b"To: " + ew_b(cs, ksx1001("김철수")) + b" <chulsoo.kim@example.co.kr>",
        b"Subject: " + ew_q(cs, ksx1001("[회의] 11월 정기")),
        b" " + ew_q(cs, ksx1001(" 회의 안건 공유")),
        b"Thread-Topic: " + ew_b(cs, ksx1001("[회의] 11월 정기 회의 안건 공유")),
        "Thread-Index: AdtEeHAmcGxhY2Vob2xkZXIga29yZWFuIGluZGV4AA==",
        "Date: Wed, 11 Nov 2026 00:00:00 +0000",
        "Message-ID: <EX1PR00MB0000EXAMPLEKR000000@EX1PR00MB0000.example.co.kr>",
        "Accept-Language: ko-KR, en-US",
        "Content-Language: ko-KR",
        "X-MS-Has-Attach:",
        "X-MS-TNEF-Correlator:",
        "Content-Type: multipart/alternative;",
        '\tboundary="%s"' % bd,
        "MIME-Version: 1.0",
        "",
        "--" + bd,
        'Content-Type: text/plain; charset="%s"' % cs,
        "Content-Transfer-Encoding: base64",
        "",
        b64(ksx1001(plain)),
        "",
        "--" + bd,
        'Content-Type: text/html; charset="%s"' % cs,
        "Content-Transfer-Encoding: quoted-printable",
        "",
        qp(ksx1001(html)),
        "",
        "--" + bd + "--",
    )


@fixture("ko-raw-8bit-subject-filename", "malformed",
         "Raw 8-bit EUC-KR bytes, with no encoding at all, in Subject and in the filename and name parameters "
         "of two attachments; only the second attachment's Content-Type carries charset=euc-kr.")
def ko_raw8():
    bd = "raw8bit-0001"
    name1 = ksx1001("견적서_11월.xls")
    name2 = ksx1001("계약서_초안.hwp")
    return msg(
        b"From: " + ew_b("euc-kr", ksx1001("홍길동")) + b" <gildong.hong@example.co.kr>",
        "To: chulsoo.kim@example.co.kr",
        b"Subject: " + ksx1001("견적서 송부의 건 (11월)"),
        "Date: " + DATE,
        "Message-ID: <raw8bit-0001@webmail.example.co.kr>",
        "MIME-Version: 1.0",
        'Content-Type: multipart/mixed; boundary="%s"' % bd,
        "",
        "--" + bd,
        "Content-Type: text/plain; charset=euc-kr",
        "Content-Transfer-Encoding: 8bit",
        "",
        ksx1001("견적서와 계약서 초안을 보내드립니다."),
        "",
        "--" + bd,
        b'Content-Type: application/octet-stream; name="' + name1 + b'"',
        b'Content-Disposition: attachment; filename="' + name1 + b'"',
        "Content-Transfer-Encoding: base64",
        "",
        b64(XLS),
        "",
        "--" + bd,
        b'Content-Type: application/haansofthwp; charset=euc-kr; name="' + name2 + b'"',
        b'Content-Disposition: attachment; filename="' + name2 + b'"',
        "Content-Transfer-Encoding: base64",
        "",
        b64(ZIP),
        "",
        "--" + bd + "--",
    )


@fixture("ko-cp949-under-euc-kr", "malformed",
         "CP949 extension syllables outside KS X 1001 (똠, 햏, 뷁) in a base64 body and an encoded-word Subject "
         "that are both labelled euc-kr.")
def ko_cp949():
    body = crlf("축하합니다! 햏자 이벤트에 당첨되셨습니다.\n경품: 똠얌꿍 밀키트 1세트\n뷁 쿠폰 코드: EX-0000\n")
    return msg(
        "From: events@shop.example.co.kr",
        "To: recipient@example.co.kr",
        b"Subject: " + ew_b("euc-kr", cp949_extension("똠방각하 햏자 이벤트 당첨 안내")),
        "Date: " + DATE,
        "Message-ID: <cp949-0001@shop.example.co.kr>",
        "MIME-Version: 1.0",
        "Content-Type: text/plain; charset=euc-kr",
        "Content-Transfer-Encoding: base64",
        "",
        b64(cp949_extension(body)),
    )


@fixture("ko-filename-encoded-word-quoted", "provider-style",
         "Attachment names as RFC 2047 words inside quoted name and filename parameters (the Outlook and "
         "Korean webmail habit): one euc-kr word, and one UTF-8 name folded into two words.")
def ko_ew_filename():
    bd = "----=_NextPart_000_0001_01DB0000.00000000"
    pdf = ew_b("euc-kr", ksx1001("견적서_2026년11월.pdf"))
    x1 = ew_b("UTF-8", "매출_현황_".encode())
    x2 = ew_b("UTF-8", "4분기.xlsx".encode())
    xlsx = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
    return msg(
        b"From: " + ew_b("euc-kr", ksx1001("홍길동")) + b" <gildong.hong@example.co.kr>",
        "To: chulsoo.kim@example.co.kr",
        b"Subject: " + ew_b("euc-kr", ksx1001("견적서 및 매출 현황 송부")),
        "Date: " + DATE,
        "Message-ID: <ewname-0001@webmail.example.co.kr>",
        "MIME-Version: 1.0",
        "Content-Type: multipart/mixed;",
        '\tboundary="%s"' % bd,
        "",
        "This is a multi-part message in MIME format.",
        "",
        "--" + bd,
        "Content-Type: text/plain;",
        '\tcharset="euc-kr"',
        "Content-Transfer-Encoding: base64",
        "",
        b64(ksx1001(crlf("견적서와 매출 현황 파일을 첨부합니다.\n확인 부탁드립니다.\n"))),
        "",
        "--" + bd,
        "Content-Type: application/pdf;",
        b'\tname="' + pdf + b'"',
        "Content-Transfer-Encoding: base64",
        "Content-Disposition: attachment;",
        b'\tfilename="' + pdf + b'"',
        "",
        b64(PDF),
        "",
        "--" + bd,
        "Content-Type: " + xlsx + ";",
        b'\tname="' + x1,
        b"\t" + x2 + b'"',
        "Content-Transfer-Encoding: base64",
        "Content-Disposition: attachment;",
        b'\tfilename="' + x1,
        b"\t" + x2 + b'"',
        "",
        b64(ZIP),
        "",
        "--" + bd + "--",
    )


@fixture("ko-filename-rfc2231-continuation", "conformant",
         "Korean filenames in RFC 2231 continuations: an euc-kr name whose segments split a double-byte "
         "character, and a Thunderbird-style UTF-8 name with an RFC 2047 name parameter beside it.")
def ko_rfc2231():
    bd = "------------rfc2231-0001"
    hwp = ksx1001("제안서_최종.hwp")
    # 제안 | 서_ + first byte of 최 | second byte of 최, 종.hwp
    cut1 = len(ksx1001("제안"))
    cut2 = len(ksx1001("제안서_")) + 1
    segs = [hwp[:cut1], hwp[cut1:cut2], hwp[cut2:]]
    pdf = "연간 일정표.pdf".encode()
    return msg(
        "From: Test Sender <sender@example.com>",
        "To: recipient@example.co.kr",
        "Subject: Proposal and schedule",
        "Date: " + DATE,
        "Message-ID: <rfc2231-0001@example.com>",
        "MIME-Version: 1.0",
        'Content-Type: multipart/mixed; boundary="%s"' % bd,
        "",
        "This is a multi-part message in MIME format.",
        "--" + bd,
        "Content-Type: text/plain; charset=UTF-8; format=flowed",
        "Content-Transfer-Encoding: 7bit",
        "",
        "Two files are attached.",
        "",
        "--" + bd,
        b"Content-Type: application/x-hwp; name*=euc-kr''" + pct(hwp),
        "Content-Disposition: attachment;",
        b" filename*0*=euc-kr''" + pct(segs[0]) + b";",
        b" filename*1*=" + pct(segs[1]) + b";",
        b" filename*2*=" + pct(segs[2]),
        "Content-Transfer-Encoding: base64",
        "",
        b64(ZIP),
        "",
        "--" + bd,
        b'Content-Type: application/pdf; name="' + ew_b("UTF-8", pdf) + b'"',
        "Content-Disposition: attachment;",
        b" filename*0*=UTF-8''" + pct(pdf[:9]) + b";",
        b" filename*1*=" + pct(pdf[9:]),
        "Content-Transfer-Encoding: base64",
        "",
        b64(PDF),
        "",
        "--" + bd + "--",
    )


@fixture("ko-undeclared-euc-kr", "malformed",
         "A text/plain body in 8-bit EUC-KR whose Content-Type declares no charset.")
def ko_undeclared():
    body = "회원님의 계정 보안 점검 안내\n아래 링크에서 비밀번호를 변경해 주세요.\nhttp://www.example.co.kr/account\n"
    return msg(
        "From: security@example.co.kr",
        "To: recipient@example.co.kr",
        "Subject: Account notice",
        "Date: " + DATE,
        "Message-ID: <undeclared-0001@example.co.kr>",
        "MIME-Version: 1.0",
        "Content-Type: text/plain",
        "Content-Transfer-Encoding: 8bit",
        "",
    ) + ksx1001(crlf(body))


@fixture("ko-utf8-labelled-euc-kr", "malformed",
         "A base64 text/html body that is UTF-8 (its own meta tag says so) under charset=euc-kr.")
def ko_mislabel():
    html = ('<html><head><meta charset="utf-8"></head><body><p>[광고] 연말 특가 이벤트</p>'
            '<p>최대 70% 할인 쿠폰을 받아 가세요.</p><p><a href="http://shop.example.co.kr/">바로 가기</a></p>'
            '</body></html>\r\n')
    return msg(
        "From: shop@shop.example.co.kr",
        "To: recipient@example.co.kr",
        b"Subject: " + ew_b("UTF-8", "[광고] 연말 특가".encode()),
        "Date: " + DATE,
        "Message-ID: <mislabel-0001@shop.example.co.kr>",
        "MIME-Version: 1.0",
        "Content-Type: text/html; charset=euc-kr",
        "Content-Transfer-Encoding: base64",
        "",
        b64(html.encode()),
    )


# --- Japanese and Chinese -------------------------------------------------


@fixture("ja-iso-2022-jp-shift-jis", "conformant",
         "Japanese alternative: ISO-2022-JP Subject, From and 7bit text/plain, and an 8-bit Shift_JIS "
         "text/html with half-width katakana.")
def ja():
    bd = "jp-alt-0001"
    plain = "各位\n\nお世話になっております。山田です。\n来週水曜日の会議は午後二時からです。\n\nよろしくお願いいたします。\n"
    html = ('<html><head><meta http-equiv="Content-Type" content="text/html; charset=Shift_JIS"></head>'
            '<body><p>各位</p><p>来週水曜日の会議は午後二時からです。</p><p>ｾﾐﾅｰ ｺｰﾄﾞ: EX-01</p></body></html>\r\n')
    return msg(
        b"From: " + ew_b("ISO-2022-JP", "山田太郎".encode("iso2022_jp")) + b" <taro.yamada@example.jp>",
        "To: team@example.jp",
        b"Subject: " + ew_b("ISO-2022-JP", "会議のお知らせ（11月）".encode("iso2022_jp")),
        "Date: " + DATE,
        "Message-ID: <jp-0001@example.jp>",
        "MIME-Version: 1.0",
        'Content-Type: multipart/alternative; boundary="%s"' % bd,
        "",
        "--" + bd,
        "Content-Type: text/plain; charset=ISO-2022-JP",
        "Content-Transfer-Encoding: 7bit",
        "",
        iso2022jp(plain),
        "--" + bd,
        "Content-Type: text/html; charset=Shift_JIS",
        "Content-Transfer-Encoding: 8bit",
        "",
        html.encode("shift_jis"),
        "--" + bd + "--",
    )


@fixture("zh-gbk-gb18030", "malformed",
         "Chinese alternative: a base64 text/plain labelled gb2312 holding a GBK-only character (喆), and a "
         "GB18030 text/html with four-byte sequences; Subject is a gb2312 B word with the same character.")
def zh_gb():
    bd = "zh-alt-0001"
    plain = crlf("各位同事：\n喆园科技新产品发布会将于下周五下午三点举行。\n地点：总部三楼会议室\n")
    html = '<html><body><p>喆园科技新产品发布会</p><p>纪念品：𠀀 字样徽章 😀</p></body></html>\r\n'
    subject = "喆园科技 新产品发布会通知".encode("gbk")
    try:
        "喆".encode("gb2312")
        raise AssertionError("喆 must be outside GB2312")
    except UnicodeEncodeError:
        pass
    assert len("𠀀".encode("gb18030")) == 4
    return msg(
        "From: events@example.com",
        "To: team@example.com",
        b"Subject: " + ew_b("gb2312", subject),
        "Date: " + DATE,
        "Message-ID: <zh-gb-0001@example.com>",
        "MIME-Version: 1.0",
        'Content-Type: multipart/alternative; boundary="%s"' % bd,
        "",
        "--" + bd,
        "Content-Type: text/plain; charset=gb2312",
        "Content-Transfer-Encoding: base64",
        "",
        b64(plain.encode("gbk")),
        "--" + bd,
        "Content-Type: text/html; charset=GB18030",
        "Content-Transfer-Encoding: base64",
        "",
        b64(html.encode("gb18030")),
        "--" + bd + "--",
    )


@fixture("zh-big5", "conformant",
         "Traditional Chinese: Subject as a big5 Q word and a quoted-printable Big5 text/plain body.")
def zh_big5():
    body = crlf("各位同仁：\n本週五下午三點於三樓會議室開會，請準時出席。\n謝謝。\n")
    return msg(
        "From: office@example.org",
        "To: staff@example.org",
        b"Subject: " + ew_q("big5", "會議通知：本週五下午三點".encode("big5")),
        "Date: " + DATE,
        "Message-ID: <big5-0001@example.org>",
        "MIME-Version: 1.0",
        "Content-Type: text/plain; charset=big5",
        "Content-Transfer-Encoding: quoted-printable",
        "",
        qp(body.encode("big5")),
    )


# --- bulk-mailer damage ---------------------------------------------------


@fixture("bulk-no-mime-version-html-as-plain", "malformed",
         "A bulk mailer's alternative with no MIME-Version field whose second part is HTML declared text/plain.")
def bulk_no_mime():
    bd = "bulk-0001"
    html = ('<html><body><table width="600"><tr><td><a href="http://deals.example.net/track?id=0001">'
            '<img src="http://deals.example.net/banner.png" alt="Weekend deals"></a></td></tr><tr><td>'
            '<a href="http://deals.example.net/unsubscribe">Unsubscribe</a></td></tr></table></body></html>')
    return msg(
        "Received: from bulk.example.net (bulk.example.net [198.51.100.7])",
        "\tby mx.example.com with ESMTP id 0001; " + DATE,
        'From: "Example Deals" <deals@bulk.example.net>',
        "To: recipient@example.com",
        "Subject: Weekend deals just for you",
        "Date: " + DATE,
        "Message-ID: <0001.bulk@bulk.example.net>",
        "X-Mailer: ExampleBulkMailer 2.0",
        'Content-Type: multipart/alternative; boundary="%s"' % bd,
        "",
        "--" + bd,
        'Content-Type: text/plain; charset="us-ascii"',
        "Content-Transfer-Encoding: 7bit",
        "",
        "Weekend deals: up to 70% off. Visit http://deals.example.net/",
        "--" + bd,
        'Content-Type: text/plain; charset="us-ascii"',
        "Content-Transfer-Encoding: 7bit",
        "",
        html,
        "--" + bd + "--",
    )


@fixture("bulk-boundary-and-cte-typos", "malformed",
         "Boundary parameters damaged by a bulk mailer (the outer one ends in a stray extra quote, the inner "
         "one has a trailing space inside its quotes that the delimiter lines lack) around parts that misspell "
         "Content-Transfer-Encoding as quoted_printable, base-64 and 8bits.")
def bulk_boundary():
    outer, inner = "----=_Part_0001_0002.0003", "----=_Alt_0001"
    html = "<p>Your coupon is attached. Value: €5.</p>\r\n".encode()
    return msg(
        "From: promo@bulk.example.net",
        "To: recipient@example.com",
        "Subject: Your coupon",
        "Date: " + DATE,
        "Message-ID: <0002.bulk@bulk.example.net>",
        "MIME-Version: 1.0",
        'Content-Type: multipart/mixed; boundary="%s""' % outer,
        "",
        "--" + outer,
        'Content-Type: multipart/alternative; boundary="%s "' % inner,
        "",
        "--" + inner,
        "Content-Type: text/plain; charset=utf-8",
        "Content-Transfer-Encoding: quoted_printable",
        "",
        "Your coupon is attached. Value: =E2=82=AC5.",
        "--" + inner,
        "Content-Type: text/html; charset=utf-8",
        "Content-Transfer-Encoding: base-64",
        "",
        b64(html),
        "--" + inner + "--",
        "",
        "--" + outer,
        "Content-Type: text/plain; charset=utf-8",
        "Content-Transfer-Encoding: 8bits",
        "",
        "Terms: one coupon per café visit.".encode(),
        "--" + outer,
        'Content-Type: application/pdf; name="coupon.pdf"',
        'Content-Disposition: attachment; filename="coupon.pdf"',
        "Content-Transfer-Encoding: base64",
        "",
        b64(PDF),
        "--" + outer + "--",
    )


@fixture("bulk-sloppy-encodings", "malformed",
         "A bulk mailer's alternative: quoted-printable text with padding after a soft break, lower-case hex, "
         "a bare-LF soft break and an escape cut by a soft break; base64 HTML in 120-column lines with "
         "embedded spaces and a trailing tab.")
def bulk_sloppy():
    bd = "sloppy-0001"
    qp_lines = (
        b"Dear customer, your order has shipped and will arrive within three busin=   \r\n"
        b"ess days. Track it at http://track.example.com/?id=3D0001.\r\n"
        b"Caf=c3=a9 latte coupon: 2 for 1.\r\n"
        b"The total is =E2=82=AC 12=\n"
        b".50 including tax.\r\n"
        b"Split escape: =E2=8=\r\n"
        b"2=AC end.\r\n"
    )
    html = ('<html><body><p>Your order has shipped.</p><p>Track it at '
            '<a href="http://track.example.com/?id=0001">track.example.com</a>.</p>'
            '<p>Café latte coupon: 2 for 1. Total €12.50 including tax.</p></body></html>\r\n').encode()
    lines = b64(html, 120)
    lines[0] = lines[0][:40] + b" " + lines[0][40:80] + b"  " + lines[0][80:]
    lines[1] = lines[1] + b"\t"
    return msg(
        "From: orders@shop.example.net",
        "To: recipient@example.com",
        "Subject: Your order has shipped",
        "Date: " + DATE,
        "Message-ID: <0003.bulk@shop.example.net>",
        "MIME-Version: 1.0",
        'Content-Type: multipart/alternative; boundary="%s"' % bd,
        "",
        "--" + bd,
        "Content-Type: text/plain; charset=utf-8",
        "Content-Transfer-Encoding: quoted-printable",
        "",
    ) + qp_lines + msg(
        "--" + bd,
        "Content-Type: text/html; charset=utf-8",
        "Content-Transfer-Encoding: base64",
        "",
        lines,
        "--" + bd + "--",
    )


@fixture("bulk-duplicate-content-type-multipart", "malformed",
         "Two Content-Type fields, a default text/plain and then the multipart/mixed the body really is, as a "
         "bulk mailer that prepends its own default writes them; the parts themselves are well-formed.")
def bulk_dup():
    bd = "dup-0001"
    html = "<html><body><p>Invoice attached. Total: € 99.00</p></body></html>\r\n".encode()
    return msg(
        "From: billing@invoices.example.net",
        "To: recipient@example.com",
        "Subject: Your invoice",
        "Date: " + DATE,
        "Message-ID: <0004.bulk@invoices.example.net>",
        "MIME-Version: 1.0",
        "Content-Type: text/plain; charset=us-ascii",
        'Content-Type: multipart/mixed; boundary="%s"' % bd,
        "",
        "--" + bd,
        "Content-Type: text/html; charset=utf-8",
        "Content-Transfer-Encoding: base64",
        "",
        b64(html),
        "--" + bd,
        'Content-Type: application/zip; name="invoice.zip"',
        'Content-Disposition: attachment; filename="invoice.zip"',
        "Content-Transfer-Encoding: base64",
        "",
        b64(ZIP),
        "--" + bd + "--",
    )


@fixture("bulk-cr-only-no-space", "malformed",
         "An old client's message with CR-only line endings throughout and no space after any header colon.")
def bulk_cr():
    return msg(
        "From:sender@example.com",
        "To:recipient@example.org",
        "Subject:Old client test",
        "Date:" + DATE,
        "Message-ID:<cr-0001@example.com>",
        "MIME-Version:1.0",
        "Content-Type:text/plain;charset=us-ascii",
        "",
        "Line one of a message written with carriage returns only.",
        "Line two.",
        eol=b"\r",
    )


@fixture("mbox-from-line", "malformed",
         "A Thunderbird-saved message that still begins with its mbox 'From - ' line and X-Mozilla-Status "
         "fields, with a '>From ' escaped line in the body.")
def mbox():
    return msg(
        "From - Wed Nov 11 09:00:00 2026",
        "X-Mozilla-Status: 0001",
        "X-Mozilla-Status2: 00000000",
        "Return-Path: <sender@example.com>",
        "From: Test Sender <sender@example.com>",
        "To: recipient@example.org",
        "Subject: Saved from a mailbox",
        "Date: " + DATE,
        "Message-ID: <mbox-0001@example.com>",
        "MIME-Version: 1.0",
        "Content-Type: text/plain; charset=us-ascii; format=flowed",
        "Content-Transfer-Encoding: 7bit",
        "",
        "The line below was escaped when the message was stored:",
        ">From the archive, with thanks.",
    )


# --- structure ------------------------------------------------------------


@fixture("list-footer-after-close", "provider-style",
         "A mailing-list message whose list manager appended its footer after the closing boundary.")
def list_footer():
    bd = "list-alt-0001"
    return msg(
        "From: Alice Example <alice@example.org>",
        "To: example-users@lists.example.org",
        "Subject: [example-users] Release candidate available",
        "Date: " + DATE,
        "Message-ID: <rc-0001@example.org>",
        "List-Id: Example users <example-users.lists.example.org>",
        "List-Unsubscribe: <mailto:example-users-leave@lists.example.org>",
        "List-Post: <mailto:example-users@lists.example.org>",
        "Precedence: list",
        "MIME-Version: 1.0",
        'Content-Type: multipart/alternative; boundary="%s"' % bd,
        "",
        "--" + bd,
        "Content-Type: text/plain; charset=us-ascii",
        "",
        "The first release candidate is available for testing.",
        "--" + bd,
        "Content-Type: text/html; charset=us-ascii",
        "",
        "<p>The first release candidate is available for testing.</p>",
        "--" + bd + "--",
        "_______________________________________________",
        "example-users mailing list -- example-users@lists.example.org",
        "To unsubscribe send an email to example-users-leave@lists.example.org",
    )


@fixture("report-dsn-bounce", "conformant",
         "A Postfix-style delivery status notification: multipart/report; report-type=delivery-status with "
         "the notice, message/delivery-status and the returned message/rfc822.")
def dsn():
    bd = "0A1B2C3D4E.1700000000/mx.example.com"
    return msg(
        "Return-Path: <>",
        "From: Mail Delivery System <MAILER-DAEMON@mx.example.com>",
        "To: sender@example.org",
        "Subject: Undelivered Mail Returned to Sender",
        "Date: " + DATE,
        "Message-Id: <20261111000000.0A1B2C3D4E@mx.example.com>",
        "Auto-Submitted: auto-replied",
        "MIME-Version: 1.0",
        "Content-Type: multipart/report; report-type=delivery-status;",
        '\tboundary="%s"' % bd,
        "",
        "This is a MIME-encapsulated message.",
        "",
        "--" + bd,
        "Content-Description: Notification",
        "Content-Type: text/plain; charset=us-ascii",
        "",
        "This is the mail system at host mx.example.com.",
        "",
        "I'm sorry to have to inform you that your message could not",
        "be delivered to one or more recipients.",
        "",
        "<nobody@example.com>: host mx.example.com[192.0.2.25] said: 550 5.1.1",
        "    <nobody@example.com>: Recipient address rejected: User unknown",
        "",
        "--" + bd,
        "Content-Description: Delivery report",
        "Content-Type: message/delivery-status",
        "",
        "Reporting-MTA: dns; mx.example.com",
        "X-Postfix-Queue-ID: 0A1B2C3D4E",
        "Arrival-Date: " + DATE,
        "",
        "Final-Recipient: rfc822; nobody@example.com",
        "Original-Recipient: rfc822;nobody@example.com",
        "Action: failed",
        "Status: 5.1.1",
        "Remote-MTA: dns; mx.example.com",
        "Diagnostic-Code: smtp; 550 5.1.1 <nobody@example.com>: Recipient address",
        "    rejected: User unknown",
        "",
        "--" + bd,
        "Content-Description: Undelivered Message",
        "Content-Type: message/rfc822",
        "",
        "From: sender@example.org",
        "To: nobody@example.com",
        "Subject: Hello",
        "Message-ID: <hello-0001@example.org>",
        "Date: " + DATE,
        "MIME-Version: 1.0",
        "Content-Type: text/plain; charset=utf-8",
        "",
        "Original body.",
        "",
        "--" + bd + "--",
    )


@fixture("report-mdn-receipt", "conformant",
         "A read receipt: multipart/report; report-type=disposition-notification with a UTF-8 Korean notice, "
         "message/disposition-notification and the original header as text/rfc822-headers.")
def mdn():
    bd = "mdn-0001"
    return msg(
        "From: chulsoo.kim@example.co.kr",
        "To: gildong.hong@example.co.kr",
        b"Subject: " + ew_b("UTF-8", "읽음 확인: 회의 자료".encode()),
        "Date: " + DATE,
        "Message-ID: <mdn-0001@example.co.kr>",
        "MIME-Version: 1.0",
        'Content-Type: multipart/report; report-type=disposition-notification; boundary="%s"' % bd,
        "",
        "--" + bd,
        "Content-Type: text/plain; charset=UTF-8",
        "Content-Transfer-Encoding: 8bit",
        "",
        "보내신 메일을 2026년 11월 11일 오전 9시에 읽었습니다.".encode(),
        "",
        "--" + bd,
        "Content-Type: message/disposition-notification",
        "",
        "Reporting-UA: mail.example.co.kr; ExampleMail",
        "Final-Recipient: rfc822;chulsoo.kim@example.co.kr",
        "Original-Message-ID: <meeting-0001@example.co.kr>",
        "Disposition: manual-action/MDN-sent-manually; displayed",
        "",
        "--" + bd,
        "Content-Type: text/rfc822-headers",
        "",
        "From: gildong.hong@example.co.kr",
        "To: chulsoo.kim@example.co.kr",
        b"Subject: " + ew_b("UTF-8", "회의 자료".encode()),
        "Message-ID: <meeting-0001@example.co.kr>",
        "Disposition-Notification-To: gildong.hong@example.co.kr",
        "",
        "--" + bd + "--",
    )


@fixture("calendar-invite", "provider-style",
         "A calendar invitation: mixed > alternative of quoted-printable text and HTML and an 8-bit "
         "text/calendar; method=REQUEST, plus the same event as a base64 application/ics attachment.")
def calendar():
    ics = crlf(
        "BEGIN:VCALENDAR\n"
        "PRODID:-//Example Corp//Example Calendar 1.0//EN\n"
        "VERSION:2.0\n"
        "CALSCALE:GREGORIAN\n"
        "METHOD:REQUEST\n"
        "BEGIN:VEVENT\n"
        "DTSTART:20261118T050000Z\n"
        "DTEND:20261118T060000Z\n"
        "DTSTAMP:20261111T000000Z\n"
        "ORGANIZER;CN=홍길동:mailto:gildong.hong@example.com\n"
        "UID:0000000000000000000000000001@example.com\n"
        "ATTENDEE;CUTYPE=INDIVIDUAL;ROLE=REQ-PARTICIPANT;PARTSTAT=NEEDS-ACTION;RSVP=\n"
        " TRUE;CN=recipient@example.org;X-NUM-GUESTS=0:mailto:recipient@example.org\n"
        "SUMMARY:주간 회의\n"
        "LOCATION:3층 회의실\n"
        "SEQUENCE:0\n"
        "STATUS:CONFIRMED\n"
        "END:VEVENT\n"
        "END:VCALENDAR\n").encode()
    plain = crlf("홍길동 님이 회의에 초대했습니다.\n주간 회의\n2026년 11월 18일 (수) 오후 2시 ~ 3시 (KST)\n"
                 "장소: 3층 회의실\n").encode()
    html = ('<div><p>홍길동 님이 회의에 초대했습니다.</p><h3>주간 회의</h3>'
            '<p>2026년 11월 18일 (수) 오후 2시 ~ 3시 (KST)</p><p>장소: 3층 회의실</p></div>\r\n').encode()
    mixed, alt = "000000000000cal0mixed0001", "000000000000cal0alt00001"
    return msg(
        "From: =?UTF-8?B?" + base64.b64encode("홍길동".encode()).decode() + "?= <gildong.hong@example.com>",
        "To: recipient@example.org",
        b"Subject: " + ew_b("UTF-8", "초대: 주간 회의 - 2026년 11월 18일".encode()),
        "Date: " + DATE,
        "Message-ID: <invite-0001@calendar.example.com>",
        "MIME-Version: 1.0",
        'Content-Type: multipart/mixed; boundary="%s"' % mixed,
        "",
        "--" + mixed,
        'Content-Type: multipart/alternative; boundary="%s"' % alt,
        "",
        "--" + alt,
        'Content-Type: text/plain; charset="UTF-8"',
        "Content-Transfer-Encoding: quoted-printable",
        "",
        qp(plain),
        "--" + alt,
        'Content-Type: text/html; charset="UTF-8"',
        "Content-Transfer-Encoding: quoted-printable",
        "",
        qp(html),
        "--" + alt,
        'Content-Type: text/calendar; charset="UTF-8"; method=REQUEST',
        "Content-Transfer-Encoding: 8bit",
        "",
        ics,
        "--" + alt + "--",
        "",
        "--" + mixed,
        'Content-Type: application/ics; name="invite.ics"',
        'Content-Disposition: attachment; filename="invite.ics"',
        "Content-Transfer-Encoding: base64",
        "",
        b64(ics),
        "--" + mixed + "--",
    )


@fixture("signed-smime-detached", "conformant",
         "An S/MIME detached signature: multipart/signed; protocol=application/pkcs7-signature over text/plain, "
         "with a base64 smime.p7s holding placeholder bytes.")
def smime():
    bd = "----SMIMEBOUNDARY0000000000000001"
    return msg(
        "From: Test Sender <sender@example.com>",
        "To: recipient@example.org",
        "Subject: Signed message",
        "Date: " + DATE,
        "Message-ID: <smime-0001@example.com>",
        "MIME-Version: 1.0",
        'Content-Type: multipart/signed; protocol="application/pkcs7-signature"; micalg=sha-256;',
        '\tboundary="%s"' % bd,
        "",
        "This is an S/MIME signed message",
        "",
        "--" + bd,
        "Content-Type: text/plain; charset=utf-8",
        "Content-Transfer-Encoding: 7bit",
        "",
        "This text is covered by the signature below.",
        "",
        "--" + bd,
        'Content-Type: application/pkcs7-signature; name="smime.p7s"',
        "Content-Transfer-Encoding: base64",
        'Content-Disposition: attachment; filename="smime.p7s"',
        "Content-Description: S/MIME Cryptographic Signature",
        "",
        b64(P7S),
        "",
        "--" + bd + "--",
    )


@fixture("signed-pgp-mime", "conformant",
         "A PGP/MIME signed message: multipart/signed; protocol=application/pgp-signature over a "
         "multipart/mixed of text and a patch attachment, with a placeholder ASCII-armoured signature.")
def pgp():
    sig, mixed = "------------pgp-sig-0001", "------------pgp-mixed-0001"
    patch = b"--- a/README\n+++ b/README\n@@ -1 +1 @@\n-old line\n+new line\n"
    return msg(
        "From: Test Sender <sender@example.com>",
        "To: recipient@example.org",
        "Subject: Patch for review",
        "Date: " + DATE,
        "Message-ID: <pgp-0001@example.com>",
        "MIME-Version: 1.0",
        "Content-Type: multipart/signed; micalg=pgp-sha256;",
        ' protocol="application/pgp-signature";',
        ' boundary="%s"' % sig,
        "",
        "This is an OpenPGP/MIME signed message (RFC 4880 and 3156)",
        "--" + sig,
        'Content-Type: multipart/mixed; boundary="%s"' % mixed,
        "",
        "--" + mixed,
        "Content-Type: text/plain; charset=UTF-8; format=flowed",
        "Content-Transfer-Encoding: 7bit",
        "",
        "The patch is attached.",
        "",
        "--" + mixed,
        'Content-Type: text/x-patch; charset=UTF-8; name="fix.patch"',
        'Content-Disposition: attachment; filename="fix.patch"',
        "Content-Transfer-Encoding: base64",
        "",
        b64(patch),
        "",
        "--" + mixed + "--",
        "",
        "--" + sig,
        'Content-Type: application/pgp-signature; name="OpenPGP_signature.asc"',
        "Content-Description: OpenPGP digital signature",
        'Content-Disposition: attachment; filename="OpenPGP_signature.asc"',
        "",
        "-----BEGIN PGP SIGNATURE-----",
        "",
        "cGxhY2Vob2xkZXIgc2lnbmF0dXJlIGJ5dGVzLCBub3QgYSByZWFsIHNpZ25hdHVyZQ==",
        "=AAAA",
        "-----END PGP SIGNATURE-----",
        "",
        "--" + sig + "--",
    )


@fixture("forwarded-rfc822-attachment", "conformant",
         "A forward as attachment: multipart/mixed with a note and a message/rfc822 whose own multipart/mixed "
         "has a base64 UTF-8 Korean body and a PDF attachment.")
def forwarded():
    outer, inner = "fwd-outer-0001", "fwd-inner-0001"
    return msg(
        "From: Test Sender <sender@example.com>",
        "To: recipient@example.org",
        "Subject: Fwd: report",
        "Date: " + DATE,
        "Message-ID: <fwd-0001@example.com>",
        "MIME-Version: 1.0",
        'Content-Type: multipart/mixed; boundary="%s"' % outer,
        "",
        "--" + outer,
        "Content-Type: text/plain; charset=utf-8",
        "",
        "Forwarding the message below.",
        "--" + outer,
        'Content-Type: message/rfc822; name="original.eml"',
        'Content-Disposition: attachment; filename="original.eml"',
        "",
        "From: =?UTF-8?B?" + base64.b64encode("김철수".encode()).decode() + "?= <chulsoo.kim@example.co.kr>",
        "To: sender@example.com",
        b"Subject: " + ew_b("UTF-8", "월간 보고서".encode()),
        "Date: " + DATE,
        "Message-ID: <report-0001@example.co.kr>",
        "MIME-Version: 1.0",
        'Content-Type: multipart/mixed; boundary="%s"' % inner,
        "",
        "--" + inner,
        "Content-Type: text/plain; charset=utf-8",
        "Content-Transfer-Encoding: base64",
        "",
        b64(crlf("월간 보고서를 첨부합니다.\n").encode()),
        "--" + inner,
        'Content-Type: application/pdf; name="report.pdf"',
        'Content-Disposition: attachment; filename="report.pdf"',
        "Content-Transfer-Encoding: base64",
        "",
        b64(PDF),
        "--" + inner + "--",
        "",
        "--" + outer + "--",
    )


@fixture("list-digest", "provider-style",
         "A Mailman-style MIME digest: a table of contents, a multipart/digest of two messages (one with an "
         "explicit message/rfc822 type, one relying on the digest default) and a footer part.")
def digest():
    outer, inner = "===============0000000000000000001==", "===============0000000000000000002=="
    return msg(
        "From: example-users-request@lists.example.org",
        "To: example-users@lists.example.org",
        "Subject: example-users Digest, Vol 12, Issue 3",
        "Date: " + DATE,
        "Message-ID: <digest-0001@lists.example.org>",
        "MIME-Version: 1.0",
        'Content-Type: multipart/mixed; boundary="%s"' % outer,
        "",
        "--" + outer,
        'Content-Type: text/plain; charset="us-ascii"',
        "Content-Description: example-users Digest, Vol 12, Issue 3",
        "",
        "Today's Topics:",
        "",
        "   1. First topic (Alice Example)",
        "   2. Second topic (Bob Example)",
        "",
        "--" + outer,
        'Content-Type: multipart/digest; boundary="%s"' % inner,
        "",
        "--" + inner,
        "Content-Type: message/rfc822",
        "MIME-Version: 1.0",
        "",
        "Message: 1",
        "Date: " + DATE,
        "From: Alice Example <alice@example.org>",
        "Subject: First topic",
        'Content-Type: text/plain; charset="us-ascii"',
        "",
        "Body of the first message.",
        "",
        "--" + inner,
        "",
        "Message: 2",
        "Date: " + DATE,
        "From: Bob Example <bob@example.org>",
        "Subject: Second topic",
        "",
        "Body of the second message.",
        "",
        "--" + inner + "--",
        "",
        "--" + outer,
        'Content-Type: text/plain; charset="us-ascii"',
        "Content-Description: Digest Footer",
        "",
        "_______________________________________________",
        "example-users mailing list",
        "",
        "--" + outer + "--",
    )


# --- truncated and adversarial --------------------------------------------


@fixture("cut-in-base64-attachment", "truncated",
         "A message cut in the middle of a base64 attachment: mid-line, inside a four-character group, "
         "with no closing delimiter and no final line break.")
def cut_b64():
    bd = "cut-0001"
    data = ZIP * 4
    text = msg(
        "From: sender@example.com",
        "To: recipient@example.org",
        "Subject: Invoice",
        "Date: " + DATE,
        "Message-ID: <cut-0001@example.com>",
        "MIME-Version: 1.0",
        'Content-Type: multipart/mixed; boundary="%s"' % bd,
        "",
        "--" + bd,
        "Content-Type: text/plain; charset=us-ascii",
        "",
        "Invoice attached.",
        "--" + bd,
        'Content-Type: application/zip; name="invoice.zip"',
        'Content-Disposition: attachment; filename="invoice.zip"',
        "Content-Transfer-Encoding: base64",
        "",
        b64(data),
    )
    attach = text.index(b"base64\r\n\r\n") + len(b"base64\r\n\r\n")
    return text[:attach + 76 + 2 + 30]


@fixture("cut-inside-header", "truncated",
         "A message cut inside its Subject field: no line ending, no empty line, no body.")
def cut_header():
    return msg(
        "Return-Path: <sender@example.com>",
        "Received: from mail.example.com (mail.example.com [192.0.2.10])",
        "\tby mx.example.org with ESMTPS id 0001",
        "\tfor <recipient@example.org>; " + DATE,
        "From: Test Sender <sender@example.com>",
        "To: recipient@example.org",
    ) + b"Subject: Quarterly rep"


@fixture("nul-in-text", "adversarial",
         "NUL bytes inside an 8-bit UTF-8 text/plain body, hiding text from C-string consumers.")
def nul():
    return msg(
        "From: sender@example.com",
        "To: recipient@example.org",
        "Subject: Visible text",
        "Date: " + DATE,
        "Message-ID: <nul-0001@example.com>",
        "MIME-Version: 1.0",
        "Content-Type: text/plain; charset=utf-8",
        "Content-Transfer-Encoding: 8bit",
        "",
        b"Visible text.\x00\x00\x00Hidden after three NULs.",
        b"Second line with a NUL\x00inside.",
    )


@fixture("subject-5000", "adversarial",
         "A 5000-character Subject on one unfolded line, five times the 998-octet line limit.")
def subject5000():
    subject = ("LIMITED OFFER " * 400)[:5000]
    assert len(subject) == 5000
    return msg(
        "From: offers@bulk.example.net",
        "To: recipient@example.com",
        "Subject: " + subject,
        "Date: " + DATE,
        "Message-ID: <long-0001@bulk.example.net>",
        "MIME-Version: 1.0",
        "Content-Type: text/plain; charset=us-ascii",
        "",
        "Body after a very long subject.",
    )


@fixture("delimiter-lookalike", "adversarial",
         "A part body holding lines that only begin like the delimiter (suffixes, a leading space, a closing "
         "delimiter followed by text) before the real next part.")
def lookalike():
    bd = "part-boundary"
    return msg(
        "From: sender@example.com",
        "To: recipient@example.org",
        "Subject: Lookalike delimiters",
        "Date: " + DATE,
        "Message-ID: <lookalike-0001@example.com>",
        "MIME-Version: 1.0",
        'Content-Type: multipart/mixed; boundary="%s"' % bd,
        "",
        "--" + bd,
        "Content-Type: text/plain; charset=us-ascii",
        "",
        "The lines below only look like delimiters:",
        "--" + bd + "-not-really",
        "--" + bd + "--but-not-closing",
        " --" + bd,
        "--" + bd + "X",
        "End of part one.",
        "--" + bd,
        "Content-Type: text/plain; charset=us-ascii",
        "",
        "Part two.",
        "--" + bd + "--",
    )


@fixture("nested-reused-boundary-deep", "adversarial",
         "A multipart/alternative inside multipart/mixed that reuses the outer boundary, followed by an "
         "attachment of the outer multipart.")
def reused():
    bd = "same-0001"
    return msg(
        "From: sender@example.com",
        "To: recipient@example.org",
        "Subject: Reused boundary",
        "Date: " + DATE,
        "Message-ID: <reused-0001@example.com>",
        "MIME-Version: 1.0",
        'Content-Type: multipart/mixed; boundary="%s"' % bd,
        "",
        "--" + bd,
        'Content-Type: multipart/alternative; boundary="%s"' % bd,
        "",
        "--" + bd,
        "Content-Type: text/plain; charset=us-ascii",
        "",
        "plain text",
        "--" + bd,
        "Content-Type: text/html; charset=us-ascii",
        "",
        "<p>html text</p>",
        "--" + bd + "--",
        "",
        "--" + bd,
        'Content-Type: application/pdf; name="a.pdf"',
        'Content-Disposition: attachment; filename="a.pdf"',
        "Content-Transfer-Encoding: base64",
        "",
        b64(PDF),
        "--" + bd + "--",
    )


# --- driver ---------------------------------------------------------------


def manifest_text(fixtures):
    with open(MANIFEST, encoding="utf-8") as f:
        m = json.load(f)
    entries = m["fixtures"]
    by_name = {e["name"]: e for e in entries}
    for name, category, description, _ in fixtures:
        e = by_name.get(name)
        if e is None:
            e = {"name": name}
            entries.append(e)
            by_name[name] = e
        e["category"] = category
        e["description"] = description
    return json.dumps(m, ensure_ascii=False, indent=2) + "\n"


def main(argv):
    check = argv[1:] == ["--check"]
    if argv[1:] and not check:
        print(__doc__, file=sys.stderr)
        return 2
    names = [f[0] for f in FIXTURES]
    assert len(names) == len(set(names)), "duplicate fixture name"
    outputs = {}
    for name, _, _, build in FIXTURES:
        data = build()
        assert len(data) <= 8192, (name, len(data))
        outputs[os.path.join(CORPUS, name + ".eml")] = data
    differ = []
    for path, data in sorted(outputs.items()):
        current = None
        if os.path.exists(path):
            with open(path, "rb") as f:
                current = f.read()
        if current == data:
            continue
        differ.append(os.path.relpath(path, ROOT))
        if not check:
            with open(path, "wb") as f:
                f.write(data)
    # The manifest: only the description and category of these fixtures.
    with open(MANIFEST, encoding="utf-8") as f:
        before = f.read()
    meta_ok = True
    m = json.loads(before)
    by_name = {e["name"]: e for e in m["fixtures"]}
    for name, category, description, _ in FIXTURES:
        e = by_name.get(name)
        if e is None or e.get("category") != category or e.get("description") != description:
            meta_ok = False
    if not meta_ok:
        differ.append(os.path.relpath(MANIFEST, ROOT))
        if not check:
            text = manifest_text(FIXTURES)
            with open(MANIFEST, "w", encoding="utf-8") as f:
                f.write(text)
    for path in differ:
        print(("differs: " if check else "wrote: ") + path)
    print("%d fixtures, %d file(s) %s" % (len(FIXTURES), len(differ), "differ" if check else "written"))
    return 1 if check and differ else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
