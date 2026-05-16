# Upstream draft — Contribution 2: Artifact-by-reference via `metadata.artifact_url`

**Target repo:** `synadia-ai/synadia-agent-sdk-docs`  
**Amends:** core-protocol.md — replaces §5.5 "Future direction: artifact
endpoint (≥ 0.3)" with a specified design, and extends §2.1 prompt-endpoint
metadata and §5.2 attachments.  
**Backward compatibility:** fully additive. The `artifact_url` metadata key
and the `artifact_uri` attachment field are both optional; v0.1/v0.2
implementations that do not set or read them remain correct.  
**Appendix B numbering:** this draft claims **B.16, B.17, B.18, B.19**
(Appendix B currently runs B.1–B.12 plus B.11a; the traceparent draft
claims B.13–B.15). Numbering is contiguous on the assumption both drafts
merge together; if they merge in a different order, renumber at submission
time.  
**Status:** draft, ready to copy-paste into an upstream PR.

---

## Motivation

The current spec caps single-message payloads at `max_payload` (commonly
1 MB, §2.1). Inline base64 attachment encoding expands binary content by
~33%, pushing even modest files over the limit. The §5.5 slot reserved a
future "attachments endpoint" backed by JetStream Object Store; that design
requires request-side streaming and a new wire contract.

This proposal adds a lighter, backward-compatible path that works *today*:
agents advertise an HTTP endpoint for large-file retrieval in their service
metadata. Callers that cannot fit a file inline can pass a URI reference
instead; agents fetch on demand. The `fossil://` URI scheme is specified for
agents backed by a Fossil content-addressable store; `https://` and other
schemes are also defined. The JetStream-backed `attachments` endpoint remains
on the roadmap for push/upload semantics.

---

## Proposed spec text

### §2.1  Prompt endpoint metadata (EDIT)

**Before:**

```json
{
  "max_payload": "1MB",
  "attachments_ok": true
}
```

| Key              | Type    | Required | Description |
|------------------|---------|----------|-------------|
| `max_payload`    | string  | Yes      | … |
| `attachments_ok` | boolean | Yes      | … |

**After — add one optional row:**

```json
{
  "max_payload": "1MB",
  "attachments_ok": true,
  "artifact_url": "https://agents.example.com/artifacts/claude-code/aconnolly/synadia-com-2"
}
```

| Key              | Type    | Required | Description                                                                                                                                               |
|------------------|---------|----------|-----------------------------------------------------------------------------------------------------------------------------------------------------------|
| `max_payload`    | string  | Yes      | Maximum single-message request payload size. Format: positive integer + `B`/`KB`/`MB`/`GB`. Callers MUST enforce locally (§5.4).                          |
| `attachments_ok` | boolean | Yes      | Whether the endpoint accepts JSON envelopes with an `attachments` array.                                                                                  |
| `artifact_url`   | string  | No       | Base URL of the agent's artifact HTTP endpoint. When present, callers MAY supply large payloads by reference (§5.5) instead of inline base64. Absence means the agent does not support artifact-by-reference; callers MUST fall back to inline or reject locally. |

The `artifact_url` value is an absolute HTTP(S) URL whose authority and path
are controlled by the agent. Callers MUST NOT construct artifact fetch URLs
by appending to this base beyond what §5.5 specifies.

---

### §3.2  Required service metadata (EDIT — illustrative only)

The existing §3.2 already states: *"Additional metadata keys MAY be included
and MUST be preserved by tools that relay service info."* That clause
already covers `artifact_url`, so no normative edit is required here.

The illustrative example below shows the new optional key alongside the
existing required ones so a maintainer can see how it slots in:

**Before:**

```json
{
  "agent": "claude-code",
  "owner": "aconnolly",
  "session": "synadia-com-2",
  "protocol_version": "0.3"
}
```

| Key                | Type   | Required           | Description |
|--------------------|--------|--------------------|-------------|
| `agent`            | string | Yes                | …           |
| `owner`            | string | Yes                | …           |
| `session`          | string | When session-aware | …           |
| `protocol_version` | string | Yes                | …           |

**After (new row appended, no required-row changes):**

```json
{
  "agent": "claude-code",
  "owner": "aconnolly",
  "session": "synadia-com-2",
  "protocol_version": "0.3",
  "artifact_url": "http://127.0.0.1:8080/fossil/synadia-com-2"
}
```

| Key                | Type   | Required           | Description                                                                                                                                                                            |
|--------------------|--------|--------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `agent`            | string | Yes                | …                                                                                                                                                                                      |
| `owner`            | string | Yes                | …                                                                                                                                                                                      |
| `session`          | string | When session-aware | …                                                                                                                                                                                      |
| `protocol_version` | string | Yes                | …                                                                                                                                                                                      |
| `artifact_url`     | string | No                 | Optional convenience mirror of the `prompt` endpoint's `artifact_url` (§2.1) at the service level, useful for tools that read service metadata but not per-endpoint metadata. Agents MAY set either or both; when both are set they MUST hold the same value. |

---

### §5.2  Attachments (EDIT)

**Before:**

```json
{ "filename": "report.pdf", "content": "<base64>" }
```

| Field      | Type   | Required | Description |
|------------|--------|----------|-------------|
| `filename` | string | Yes      | … |
| `content`  | string | Yes      | … |

**After — `content` becomes conditionally required; add `artifact_uri`:**

```json
{ "filename": "report.pdf", "content": "<base64>" }
```

```json
{ "filename": "report.pdf", "artifact_uri": "fossil://c7d8e9f0123456789abcdef0fedcba9876543210/report.pdf" }
```

| Field          | Type   | Required      | Description                                                                                                                                              |
|----------------|--------|---------------|----------------------------------------------------------------------------------------------------------------------------------------------------------|
| `filename`     | string | Yes           | Authoritative file name. Agents interpret bytes by extension or content sniff.                                                                           |
| `content`      | string | Cond.         | Standard-alphabet, padded base64 (RFC 4648 §4). MUST NOT use URL-safe encoding or whitespace. Required when `artifact_uri` is absent.                    |
| `artifact_uri` | string | Cond.         | URI referencing the file in a content-addressable store (§5.5). Required when `content` is absent. Callers MUST NOT send `artifact_uri` unless the agent's `artifact_url` metadata is set. |

Exactly one of `content` or `artifact_uri` MUST be present. An attachment
that carries both is malformed; agents SHOULD respond `400`.

---

### §5.5  Artifact endpoint (EDIT — replaces "Future direction" stub)

**Before:**

> A future revision will define the `attachments` endpoint at
> `agents.attachments.{agent}.{owner}.{name}` (v0.3 verb-first) for
> large-file upload:
> …
> Precise wire format, chunking, lifetime, and reference-handoff semantics
> are deferred. v0.1 implementations SHOULD structure attachment handling so
> that adding the `attachments`-endpoint code path is additive, not a
> rewrite.

**After — replace in full:**

---

#### 5.5  Artifact-by-reference

This section defines the *pull* path: a caller that has a large file
accessible over HTTP passes a URI instead of inline base64. The agent fetches
the file at or before the point it needs to read the attachment content.

The complementary *push* path (caller uploads to the agent before sending the
`prompt`) is reserved for a future `attachments` endpoint (§2 subject table)
and is not defined here.

##### 5.5.1  Capability advertisement

An agent that supports artifact-by-reference MUST set `artifact_url` in its
`prompt` endpoint metadata (§2.1). Absence of the key means the agent does
not support this feature; callers MUST NOT send `artifact_uri` fields to such
agents.

##### 5.5.2  URI schemes

| Scheme    | Semantics                                                                                                                          | Fetch method                                                        |
|-----------|------------------------------------------------------------------------------------------------------------------------------------|---------------------------------------------------------------------|
| `https`   | Standard HTTPS GET. The agent fetches `artifact_uri` directly.                                                                     | HTTP GET, following redirects.                                      |
| `http`    | HTTP GET (use only on trusted private networks).                                                                                   | HTTP GET, following redirects.                                      |
| `fossil`  | Content-addressable blob in a Fossil repository. Format: `fossil://<commit-hash>/<path>`. The agent resolves via `artifact_url`.   | Agent appends `/<commit-hash>/<path>` to its `artifact_url` base.   |
| (other)   | Opaque; future schemes may be defined.                                                                                             | Agents MUST return `400` for unrecognised schemes they cannot fetch.|

Callers MUST treat unknown schemes as opaque and MUST NOT construct fetch
URLs for them. Agents that cannot handle an `artifact_uri` scheme MUST
respond with a `400` error (§9.2).

##### 5.5.3  `fossil://` scheme details

The `fossil://` URI is designed for agents backed by a Fossil
content-addressable store exposed over HTTP:

```
fossil://<commit-hash>/<path-within-tree>
```

- `<commit-hash>` — the full 40-hex-char (SHA-1) or 64-hex-char (SHA-3-256)
  Fossil artifact hash.
- `<path-within-tree>` — file path relative to the repository root, using
  forward slashes.

The agent MUST resolve the URI by issuing an HTTP GET to:

```
{artifact_url}/{commit-hash}/{path-within-tree}
```

where `{artifact_url}` is taken from the agent's own `prompt` endpoint
metadata. The server at `artifact_url` MUST return the raw file bytes with
status `200`, or `404` if the artifact is not present.

Example:

```
artifact_url (from §2.1 metadata):
  http://127.0.0.1:8080/fossil/synadia-com-2

artifact_uri (from §5.2 attachment):
  fossil://c7d8e9f0123456789abcdef0fedcba9876543210/report.pdf

Resolved fetch URL:
  http://127.0.0.1:8080/fossil/synadia-com-2/c7d8e9f0123456789abcdef0fedcba9876543210/report.pdf
```

##### 5.5.4  Agent fetch obligations

When a request arrives with one or more `artifact_uri` attachments:

1. The agent MUST fetch all referenced artifacts before beginning to process
   the prompt, unless the agent's implementation streams processing
   concurrently with fetching (in which case it MUST ensure each artifact is
   available before it is accessed).
2. If any fetch fails (network error, 404, unsupported scheme), the agent
   MUST respond with a `400` or `503` error (§9.2) and MUST NOT produce
   partial output silently.
3. The agent MUST NOT cache fetched artifacts across unrelated sessions unless
   it can guarantee the content is immutable (which `fossil://` commit-hash
   references inherently are).

##### 5.5.5  Backward compatibility

All changes in this section are backward-compatible:

- `artifact_url` in endpoint metadata is optional; existing agents need not
  set it.
- `artifact_uri` in attachment objects is optional; existing callers need not
  send it.
- Agents that do not implement this section continue to work because callers
  MUST check for `artifact_url` before sending `artifact_uri` (§5.4 extended
  validation).
- The `attachments` endpoint verb (§2) remains reserved for the future push
  path; nothing in this section conflicts with it.

---

### §5.4  Client-side validation (EDIT — add one bullet)

Add after the existing two bullets:

> - If any attachment contains `artifact_uri` and the endpoint's
>   `artifact_url` metadata is absent or empty, the caller MUST fail locally
>   without publishing.

---

## Worked wire example (Appendix B style)

### B.16  Service info with `artifact_url`

`$SRV.INFO.agents` response (relevant excerpt):

```json
{
  "name": "agents",
  "metadata": {
    "agent": "claude-code",
    "owner": "aconnolly",
    "session": "synadia-com-2",
    "protocol_version": "0.3",
    "artifact_url": "http://127.0.0.1:8080/fossil/synadia-com-2"
  },
  "endpoints": [
    {
      "name": "prompt",
      "subject": "agents.prompt.claude-code.aconnolly.synadia-com-2",
      "queue_group": "agents",
      "metadata": {
        "max_payload": "1MB",
        "attachments_ok": true,
        "artifact_url": "http://127.0.0.1:8080/fossil/synadia-com-2"
      }
    }
  ]
}
```

### B.17  Request with `artifact_uri` attachment

The caller has a 15 MB PDF stored in the agent's Fossil repo. Sending it
inline would exceed `max_payload=1MB`. The caller sends a reference instead.

Published to `agents.prompt.claude-code.aconnolly.synadia-com-2`:

```json
{
  "prompt": "summarize the attached report",
  "attachments": [
    {
      "filename": "report.pdf",
      "artifact_uri": "fossil://c7d8e9f0123456789abcdef0fedcba9876543210/report.pdf"
    }
  ]
}
```

The agent resolves the artifact:

```
GET http://127.0.0.1:8080/fossil/synadia-com-2/c7d8e9f0123456789abcdef0fedcba9876543210/report.pdf
→ 200 OK, Content-Type: application/pdf, body: <15 MB PDF bytes>
```

The agent then processes the prompt with the full file content.

### B.18  `https://` artifact URI

For a file hosted on a generic HTTPS server:

```json
{
  "prompt": "translate to French",
  "attachments": [
    {
      "filename": "document.txt",
      "artifact_uri": "https://storage.example.com/artifacts/uuid-1234/document.txt"
    }
  ]
}
```

The agent issues:

```
GET https://storage.example.com/artifacts/uuid-1234/document.txt
→ 200 OK, body: <text bytes>
```

### B.19  Error: unsupported scheme

If the caller sends `artifact_uri: "s3://my-bucket/file.bin"` to an agent
that only understands `fossil://` and `https://`, the agent responds:

Headers:
```
Nats-Service-Error-Code: 400
Nats-Service-Error: unsupported artifact_uri scheme
```

Body:
```json
{
  "error": "unsupported_artifact_scheme",
  "message": "scheme 's3' is not supported by this agent; use fossil:// or https://"
}
```

Followed by the empty-payload terminator (B.9).

---

## References

- RFC 4648 §4 — Base64 encoding
- §5.5 (current spec) — reserved `attachments` endpoint stub
- §2.1 — prompt endpoint metadata
- §5.4 — client-side validation
- Fossil SCM HTTP API: https://fossil-scm.org/home/doc/trunk/www/server/
