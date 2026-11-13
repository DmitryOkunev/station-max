#!/usr/bin/env python3
"""Test LLM phrase-streaming without the speaker.

Emulates the quasar-bridge native path end-to-end, minus the station:
    routing rule -> system prompt -> StreamComplete SSE -> sentenceChunker
    -> TTSReplacer -> per-phrase timing (as the station would speak them).

Configs are read from the same files the bridge uses:
    <dir>/config.json  (routing rules, system prompts, tts_replacements)
    <dir>/llm.json     (model, endpoint, api key, chat parameters)

Default dir: <repo>/configs/quasar-bridge.  To test the LIVE config deployed
on the bridge server:  python3 test-llm-stream.py -d /opt/quasar-bridge

Examples:
    ./scripts/test-llm-stream.py                                  # default query, auto rule
    ./scripts/test-llm-stream.py -r slysh "ты чё, умный?"
    ./scripts/test-llm-stream.py -r podskazhi "как варить борщ"
    ./scripts/test-llm-stream.py --raw --no-stream "привет"
"""
import argparse
import json
import os
import re
import sys
import time
import urllib.error
import urllib.request

SCRIPT_DIR = os.path.dirname(os.path.abspath(__file__))
REPO_DIR = os.path.dirname(SCRIPT_DIR)
DEFAULT_DIR = os.path.join(REPO_DIR, "configs", "quasar-bridge")

# Mirrors llm_stream.go: newSentenceChunker().minRunes
MIN_RUNES = 25

C_G, C_Y, C_R, C_C, C_0 = "\033[1;32m", "\033[1;33m", "\033[1;31m", "\033[1;36m", "\033[0m"


def die(msg, code=1):
    print(f"{C_R}[-] {msg}{C_0}", file=sys.stderr)
    sys.exit(code)


def load_json(path):
    try:
        with open(path, encoding="utf-8") as f:
            return json.load(f)
    except FileNotFoundError:
        die(f"config not found: {path}")
    except json.JSONDecodeError as e:
        die(f"invalid JSON in {path}: {e}")


# ── Rule selection (routing.go emulation) ────────────────────────────────────

def pick_rule(cfg, rule_name, text):
    """Return (rule_dict, matched_by). rule_name='auto' matches regexes in order."""
    rules = cfg.get("routing", {}).get("rules", [])
    if rule_name != "auto":
        for r in rules:
            if r.get("name") == rule_name:
                return r, "explicit"
        die(f"rule '{rule_name}' not found in config.json routing.rules")
    for r in rules:
        if r.get("route") != "llm":
            continue
        rx = r.get("match", {}).get("regex")
        if not rx:
            continue
        for flags in (0, re.IGNORECASE):  # exact first, then case-insensitive
            try:
                if re.match(rx, text, flags):
                    return r, "regex" if flags == 0 else "regex (case-insensitive)"
            except re.error:
                break
    # No regex hit: emulate the classifier fallback (routing.classifier.on_error
    # is yandex, but for a manual test we default to the roast rule).
    for r in rules:
        if r.get("name") == "ask_llm_slysh":
            return r, "fallback (no regex matched, classifier not emulated)"
    die("no llm rule found in config.json")


# ── TTSReplacer emulation (tts_matrix.go, exact overrides only) ──────────────

def tts_replace(text, matrix):
    if "*" not in text or not matrix:
        return text
    exact = {k.strip().lower(): v.strip() for k, v in matrix.items() if k.strip() and v.strip()}

    def sub(m):
        tok = m.group(0)
        return exact.get(tok.lower(), tok)

    return re.sub(r"[а-яёА-ЯЁa-zA-Z*]+", sub, text)


# ── sentenceChunker emulation (llm_stream.go) ────────────────────────────────

def _boundary(text):
    for i, ch in enumerate(text):
        if ch in ".!?\n…":
            after = i + 1
            if after >= len(text) or text[after] in " \n":
                return i
    return -1


class SentenceChunker:
    def __init__(self):
        self.buf = ""

    def feed(self, tok):
        self.buf += tok
        return self._flush(final=False)

    def flush(self):
        return self._flush(final=True)

    def _flush(self, final):
        text, out = self.buf, []
        while True:
            idx = _boundary(text)
            if idx < 0:
                break
            sentence = text[: idx + 1].strip()
            text = text[idx + 1:]
            if len(sentence) < MIN_RUNES and not final and text.strip():
                text = sentence + " " + text  # too short – accumulate more
                break
            if sentence:
                out.append(sentence)
        self.buf = "" if final else text
        if final and text.strip():
            out.append(text.strip())
        return out


# ── StreamComplete emulation (llm_stream.go) ─────────────────────────────────

def build_request(model, messages, stream, max_tokens, reasoning_effort, chat_parameters):
    request = {"model": model, "messages": messages, "stream": stream, "temperature": 0}
    if max_tokens > 0:
        request["max_tokens"] = max_tokens
    if reasoning_effort:
        request["reasoning_effort"] = reasoning_effort
    for key, value in (chat_parameters or {}).items():
        if value is None:
            request.pop(key, None)
        else:
            request[key] = value
    return request


def stream_complete(base_url, api_key, request, timeout, raw):
    """Yield ('content'|'reasoning'|'usage', payload) events; raise on HTTP errors."""
    endpoint = base_url.rstrip("/") + "/chat/completions"
    body = json.dumps(request, ensure_ascii=False).encode("utf-8")
    req = urllib.request.Request(endpoint, data=body, headers={
        "Content-Type": "application/json",
        "Accept": "text/event-stream",
        **({"Authorization": "Bearer " + api_key} if api_key else {}),
    })
    try:
        resp = urllib.request.urlopen(req, timeout=timeout)
    except urllib.error.HTTPError as e:
        snippet = e.read(400).decode("utf-8", "replace")
        die(f"LLM stream returned HTTP {e.code}: {snippet}")
    except urllib.error.URLError as e:
        die(f"LLM stream transport failed: {e.reason}")

    data_buf = ""

    def flush_event():
        payload = data_buf.strip()
        if not payload or payload == "[DONE]":
            return
        try:
            chunk = json.loads(payload)
        except json.JSONDecodeError:
            return  # tolerate unknown event types
        for ch in chunk.get("choices", []):
            delta = ch.get("delta") or {}
            if delta.get("content"):
                yield "content", delta["content"]
            if delta.get("reasoning_content"):
                yield "reasoning", delta["reasoning_content"]
            fr = ch.get("finish_reason")
            if fr == "length":
                raise RuntimeError("LLM answer exceeded token budget (finish_reason=length)")
            elif fr:
                yield "finish", fr
        if chunk.get("usage"):
            yield "usage", chunk["usage"]

    gen = None
    with resp:
        for raw_line in resp:
            line = raw_line.decode("utf-8", "replace").rstrip("\r\n")
            if raw:
                print(f"{C_Y}  sse| {line[:200]}{C_0}")
            if line == "":
                gen = flush_event() if data_buf else None
                if gen is not None:
                    yield from gen
                data_buf = ""
                continue
            field, _, value = line.partition(":")
            if field != "data":
                continue
            if data_buf:
                data_buf += "\n"
            data_buf += value.lstrip(" ")
    if data_buf:
        yield from flush_event()


# ── Main ──────────────────────────────────────────────────────────────────────

def main():
    ap = argparse.ArgumentParser(description="Test quasar-bridge LLM phrase streaming (no speaker needed)")
    ap.add_argument("query", nargs="?", default="почему блять земля плоская, а луна квадратная?")
    ap.add_argument("-r", "--rule", default="auto", choices=["auto", "ask_llm_slysh", "ask_llm_podskazhi"])
    ap.add_argument("-d", "--dir", default=DEFAULT_DIR, help="config dir (default: repo configs; use /opt/quasar-bridge for live)")
    ap.add_argument("--raw", action="store_true", help="print raw SSE events")
    ap.add_argument("--no-stream", action="store_true", help="emulate non-streaming Complete() fallback")
    ap.add_argument("--timeout", type=int, default=90)
    ap.add_argument("--max-tokens", type=int, help="override max_tokens from llm.json")
    ap.add_argument("--model", help="override model from llm.json")
    ap.add_argument("--no-tts", action="store_true", help="skip TTS replacements")
    args = ap.parse_args()

    cfg = load_json(os.path.join(args.dir, "config.json"))
    llm = load_json(os.path.join(args.dir, "llm.json"))

    rule, matched = pick_rule(cfg, args.rule, args.query.strip())
    is_slysh = rule.get("name") == "ask_llm_slysh"

    # engine.go fallbackWithPrompt: slysh -> e.LLM, podskazhi -> e.Assistant
    if is_slysh:
        base = llm.get("llm_base_url", "")
        model = args.model or llm.get("llm_model", "")
        api_key = llm.get("llm_api_key", "")
        max_tokens = args.max_tokens or int(llm.get("llm_max_output_tokens", 4096) or 0)
        chat_params = llm.get("llm_chat_parameters") or {}
        api = llm.get("llm_api", "chat_completions")
    else:
        base = llm.get("assistant_base_url", "")
        model = args.model or llm.get("assistant_model", "")
        api_key = llm.get("assistant_api_key", "")
        max_tokens = args.max_tokens or int(llm.get("assistant_max_output_tokens", 4096) or 0)
        chat_params = llm.get("assistant_chat_parameters") or {}
        api = llm.get("assistant_api", "chat_completions")

    print(f"{C_C}=== quasar-bridge LLM stream test ==={C_0}")
    print(f"  config dir : {args.dir}")
    print(f"  rule       : {C_G}{rule.get('name')}{C_0} ({matched}, route={rule.get('route')})")
    print(f"  endpoint   : {base.rstrip('/')}/chat/completions ({api})")
    print(f"  model      : {model}  stream={'false' if args.no_stream else 'true'}"
          f"  max_tokens={max_tokens}"
          f"  chat_params={json.dumps(chat_params, ensure_ascii=False) if chat_params else '{}'}")
    prompt = rule.get("system_prompt", "")
    print(f"  system     : {len(prompt)} chars")
    print(f"  query      : {args.query}")
    if not is_slysh:
        print(f"  tts        : (podskazhi – default emotion)")
    else:
        print(f"  tts        : emotion=evil (station-side, ask_llm_slysh)")
    print()

    if api != "chat_completions":
        die(f"this test emulates chat_completions only; config uses '{api}'")
    if not base or not model:
        die("llm_base_url / llm_model missing in llm.json")

    messages = [{"role": "system", "content": prompt}, {"role": "user", "content": args.query}]
    request = build_request(model, messages, not args.no_stream, max_tokens,
                            llm.get("llm_reasoning_effort", ""), chat_params)

    chunker = SentenceChunker()
    t0 = time.monotonic()
    full, reasoning_chars, n_content = [], 0, 0
    phrase_no, first_t, finish, usage = 0, None, None, None

    def emit_phrases(phrases):
        nonlocal phrase_no, first_t
        for s in phrases:
            phrase_no += 1
            t = time.monotonic() - t0
            if first_t is None:
                first_t = t
            print(f"  {C_G}[+{t:6.2f}s]{C_0} фраза {phrase_no}: {s}")

    try:
        if args.no_stream:
            request["stream"] = False
            body = json.dumps(request, ensure_ascii=False).encode("utf-8")
            req = urllib.request.Request(base.rstrip("/") + "/chat/completions", data=body, headers={
                "Content-Type": "application/json",
                **({"Authorization": "Bearer " + api_key} if api_key else {}),
            })
            with urllib.request.urlopen(req, timeout=args.timeout) as resp:
                data = json.loads(resp.read().decode("utf-8", "replace"))
            answer = (data.get("choices") or [{}])[0].get("message", {}).get("content", "") or ""
            usage = data.get("usage")
            emit_phrases(chunker.feed(answer))
            emit_phrases(chunker.flush())
        else:
            for kind, payload in stream_complete(base, api_key, request, args.timeout, args.raw):
                if kind == "content":
                    n_content += 1
                    full.append(payload)
                    emit_phrases(chunker.feed(payload))
                elif kind == "reasoning":
                    reasoning_chars += len(payload)
                elif kind == "finish":
                    finish = payload
                elif kind == "usage":
                    usage = payload
            emit_phrases(chunker.flush())
    except RuntimeError as e:
        print()
        die(str(e))
    except urllib.error.URLError as e:
        print()
        die(f"stream aborted: {e.reason}")

    answer = "".join(full).strip()
    total = time.monotonic() - t0
    print()
    print(f"{C_C}=== summary ==={C_0}")
    print(f"  phrases    : {phrase_no} за {total:.2f}s (первая через {first_t:.2f}s)" if phrase_no else
          f"  phrases    : 0")
    print(f"  content    : {len(answer)} chars, {n_content} SSE deltas")
    print(f"  finish     : {finish or 'n/a'}")
    if reasoning_chars:
        print(f"  reasoning  : {reasoning_chars} chars DISCARDED (не озвучивается, но тратит время/токены)")
    if usage:
        ct = usage.get("completion_tokens") or usage.get("output_tokens") or "?"
        rt = (usage.get("completion_tokens_details") or {}).get("reasoning_tokens")
        print(f"  usage      : completion_tokens={ct}" + (f", reasoning_tokens={rt}" if rt is not None else ""))
    tts_answer = answer if args.no_tts else tts_replace(answer, cfg.get("tts_replacements"))
    if "*" in tts_answer and not args.no_tts:
        print(f"  {C_Y}warn: '*' остался в тексте после TTS-замен{C_0}")
    if "**" in answer:
        print(f"  {C_Y}warn: markdown '**' попал в ответ — TTS прочитает звёздочки (пункт 9 промпта нарушен){C_0}")
    if not answer:
        die("пустой ответ — колонка промолчала бы", code=2)
    print(f"  as-tts     : {tts_answer[:300]}{'…' if len(tts_answer) > 300 else ''}")
    print(f"{C_G}[+] OK{C_0}" if phrase_no else f"{C_Y}[!] ответ получен, но фраз не нарезано{C_0}")


if __name__ == "__main__":
    main()
