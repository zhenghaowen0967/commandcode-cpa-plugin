"""loopback HTTP 探针的底层收发：只连回环地址，只保留白名单里的UUID。

- 连接固定指向调用方已校验的回环 host/port，不跟随重定向，不使用任何凭据日志。
- 响应头只提取白名单里的 trace UUID，其他头（含cookie/authorization）一律丢弃。
- 响应体完整读入内存；超过 MAX_RESPONSE_BYTES 直接判为“不完整”，不把截断当成功。
- 只发送一次、不自动重试；失败原样上报，不取消已经发出的响应。
"""
import http.client
import json

from evidence import MAX_RESPONSE_BYTES, UUID_SEARCH_RE

# 只在这些头部里找可信的请求追踪ID，其余头部不落盘。
TRACE_HEADERS = ('x-cpa-trace-id',)


def _extract_trace_ids(response):
    found = []
    for name in TRACE_HEADERS:
        value = response.getheader(name, '')
        if value:
            found.extend(UUID_SEARCH_RE.findall(value))
    return found


def post_once(host, port, path, key, body, timeout=180):
    """POST一次并完整读完响应；返回 (status, content_type, raw, trace_ids)。"""
    conn = http.client.HTTPConnection(host, port, timeout=timeout)
    try:
        conn.request('POST', path, json.dumps(body).encode('utf-8'),
                     {'Authorization': 'Bearer ' + key,
                      'Content-Type': 'application/json',
                      'anthropic-version': '2023-06-01'})
        response = conn.getresponse()
        trace_ids = _extract_trace_ids(response)
        raw = response.read(MAX_RESPONSE_BYTES + 1)
        if len(raw) > MAX_RESPONSE_BYTES:
            raise ValueError('response_exceeds_complete_capture_limit')
        return response.status, response.getheader('Content-Type', ''), raw, trace_ids
    finally:
        conn.close()


def decode_message(raw):
    """解析非流式 Claude 消息响应；缺字段、含工具调用、异常结束都判为不完整。"""
    try:
        obj = json.loads(raw)
    except (json.JSONDecodeError, UnicodeDecodeError) as exc:
        raise ValueError('response_not_json') from exc
    if not isinstance(obj, dict) or obj.get('type') != 'message':
        raise ValueError('unexpected_reply_type')
    blocks = obj.get('content')
    if not isinstance(blocks, list) or not blocks or any(not isinstance(b, dict) for b in blocks):
        raise ValueError('response_blocks_invalid')
    if any(b.get('type') not in ('text', 'thinking', 'redacted_thinking') for b in blocks):
        raise ValueError('probe_must_not_call_tools_or_unknown_blocks')
    texts = [b.get('text') for b in blocks if b.get('type') == 'text']
    if not texts or any(not isinstance(text, str) for text in texts):
        raise ValueError('response_text_invalid')
    stop = obj.get('stop_reason')
    if stop not in ('end_turn', 'stop_sequence'):
        raise ValueError('response_not_complete')
    text = ''.join(texts)
    usage = obj.get('usage', {})
    if not isinstance(usage, dict):
        raise ValueError('response_usage_invalid')
    for name in ('input_tokens', 'output_tokens'):
        value = usage.get(name)
        if value is not None and (isinstance(value, bool) or not isinstance(value, int) or value < 0):
            raise ValueError('response_usage_invalid')
    return {
        'stop_reason': stop,
        'text': text,
        'text_characters': len(text),
        'new_tool_calls': [],
        'usage_summary': {k: usage[k] for k in ('input_tokens', 'output_tokens') if k in usage},
    }
