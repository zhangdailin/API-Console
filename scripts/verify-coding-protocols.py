#!/usr/bin/env python3
"""Bounded live probes against a private, isolated gateway verification instance.

Run on the deployment host. Credentials never enter the report. The caller must
provide a loopback-only binary; setup clones only account/model keys, strips
refresh credentials, and disables expired accounts. Cleanup removes this run's
Redis namespace and stops only the recorded temporary process.
"""
import datetime
import hashlib
import json
import os
from pathlib import Path
import secrets
import signal
import socket
import subprocess
import sys
import time
import urllib.error
import urllib.request

STAGE = Path(os.environ.get('PROTOCOL_VERIFY_DIR', '/tmp/api-console-protocol-verify-20261002'))
BASE = 'http://127.0.0.1:13002'
CHANNELS = ('workbuddy', 'cline', 'grok')


class Redis:
    def __init__(self, cfg):
        host, port = cfg.get('redis_addr', '127.0.0.1:6379').rsplit(':', 1)
        self.sock = socket.create_connection((host, int(port)), timeout=10)
        self.file = self.sock.makefile('rb')
        if cfg.get('redis_password'):
            self.command('AUTH', cfg['redis_password'])
        self.command('SELECT', cfg.get('redis_db', 0))

    def read(self):
        line = self.file.readline()
        kind, value = line[:1], line[1:-2]
        if kind == b'-':
            raise RuntimeError('Redis command failed')
        if kind == b'+':
            return value
        if kind == b':':
            return int(value)
        if kind == b'$':
            size = int(value)
            if size == -1:
                return None
            result = self.file.read(size)
            self.file.read(2)
            return result
        if kind == b'*':
            return [self.read() for _ in range(int(value))]
        raise RuntimeError('Invalid Redis response')

    def command(self, *args):
        parts = [a if isinstance(a, bytes) else str(a).encode() for a in args]
        wire = b'*%d\r\n' % len(parts)
        for part in parts:
            wire += b'$%d\r\n' % len(part) + part + b'\r\n'
        self.sock.sendall(wire)
        return self.read()

    def keys(self, pattern):
        cursor = 0
        result = []
        while True:
            cursor, batch = self.command('SCAN', cursor, 'MATCH', pattern, 'COUNT', 200)
            result.extend(batch)
            if int(cursor) == 0:
                return result


def save_private(path, value):
    path.write_text(json.dumps(value, ensure_ascii=False, indent=2))
    path.chmod(0o600)


def load_state():
    return json.loads((STAGE / 'state.json').read_text())


def http(path, body=None, key=None, admin=False, timeout=80):
    state = load_state()
    headers = {'Content-Type': 'application/json'}
    if admin:
        headers['X-Admin-Token'] = state['admin_token']
    elif key or state.get('key'):
        headers['Authorization'] = 'Bearer ' + (key or state['key'])
    request = urllib.request.Request(BASE + path, data=None if body is None else json.dumps(body).encode(), headers=headers)
    started = time.monotonic()
    try:
        response = urllib.request.urlopen(request, timeout=timeout)
    except urllib.error.HTTPError as exc:
        response = exc
    with response:
        raw = response.read(2_000_000)
        status = response.status
    try:
        data = json.loads(raw)
    except ValueError:
        data = {'_wire': raw.decode(errors='replace')}
    return status, data, round(time.monotonic() - started, 3)


def setup():
    STAGE.mkdir(mode=0o700, parents=True, exist_ok=True)
    STAGE.chmod(0o700)
    if (STAGE / 'state.json').exists():
        raise RuntimeError('Existing verification state must be cleaned first')
    source_path = Path('/opt/orchids-2api/config.json')
    cfg = json.loads(source_path.read_text())
    redis = Redis(cfg)
    source_prefix = cfg.get('redis_prefix', 'orchids:').rstrip(':') + ':'
    stored = redis.command('GET', source_prefix + 'settings:config')
    if stored:
        cfg.update(json.loads(stored))
    prefix = 'protocol-verify:' + secrets.token_hex(8) + ':'
    admin = secrets.token_urlsafe(32)
    env = os.environ.copy()
    pid = subprocess.check_output(['systemctl', 'show', 'orchids-2api.service', '-p', 'MainPID', '--value']).decode().strip()
    for pair in Path('/proc/' + pid + '/environ').read_bytes().split(b'\0'):
        if pair.startswith(b'ORCHIDS_CREDENTIAL_ENCRYPTION_KEY='):
            env['ORCHIDS_CREDENTIAL_ENCRYPTION_KEY'] = pair.split(b'=', 1)[1].decode()
    keyfile = Path(cfg.get('credential_encryption_key_file') or 'data/credential.key')
    if not keyfile.is_absolute():
        keyfile = source_path.parent / keyfile
    if not keyfile.exists() and 'ORCHIDS_CREDENTIAL_ENCRYPTION_KEY' not in env:
        raise RuntimeError('Production encryption key unavailable')
    cfg.update(port='13002', redis_prefix=prefix, admin_token=admin, admin_pass=secrets.token_urlsafe(32),
               credential_encryption_key_file=str(keyfile), debug_enabled=False, verbose_diagnostics=False,
               media_dir=str(STAGE / 'media'), deployment_instance_id=prefix.rstrip(':'), request_timeout=75,
               max_retries=0, anonymous_allow_ips=[], shared_refusal_wait_budget_ms=5000)
    save_private(STAGE / 'config.json', cfg)
    state = {'prefix': prefix, 'admin_token': admin, 'source_pid': int(pid), 'copied': 0, 'accounts': {}}
    save_private(STAGE / 'state.json', state)
    now = datetime.datetime.now(datetime.timezone.utc)
    for pattern in ('accounts:*', 'models:*'):
        for key in redis.keys(source_prefix + pattern):
            suffix = key.decode()[len(source_prefix):]
            if suffix.startswith('accounts:') and suffix not in ('accounts:ids', 'accounts:enabled', 'accounts:next_id') and not suffix.startswith('accounts:id:'):
                continue
            newkey = prefix + suffix
            if suffix.startswith('accounts:id:'):
                account = json.loads(redis.command('GET', key))
                channel = account.get('account_type', '').lower()
                for field in ('refresh_token', 'client_cookie', 'token', 'oauth_refresh_token', 'workbuddy_refresh_token', 'cline_refresh_token'):
                    account[field] = ''
                expiry_name = {'workbuddy':'workbuddy_expires_at','cline':'cline_expires_at','grok':'oauth_expires_at'}.get(channel)
                expiry = account.get(expiry_name, '') if expiry_name else ''
                if expiry and not expiry.startswith('0001-'):
                    expires = datetime.datetime.fromisoformat(expiry.replace('Z', '+00:00'))
                    if expires <= now + datetime.timedelta(minutes=3):
                        account['enabled'] = False
                counts = state['accounts'].setdefault(channel, {'total':0,'eligible_snapshot':0})
                counts['total'] += 1
                counts['eligible_snapshot'] += bool(account.get('enabled'))
                redis.command('SET', newkey, json.dumps(account), 'EX', 3600)
            else:
                dump = redis.command('DUMP', key)
                if dump:
                    redis.command('RESTORE', newkey, 3600000, dump, 'REPLACE')
            state['copied'] += 1
    log = open(STAGE / 'server.log', 'wb')
    process = subprocess.Popen([str(STAGE / 'orchids-server'), '-config', str(STAGE / 'config.json')], cwd=STAGE, env=env, stdout=log, stderr=log, start_new_session=True)
    state['pid'] = process.pid
    state['binary_sha256'] = hashlib.sha256((STAGE / 'orchids-server').read_bytes()).hexdigest()
    save_private(STAGE / 'state.json', state)
    for _ in range(30):
        try:
            status, data, _ = http('/health', timeout=2)
            if status == 200:
                break
        except (OSError, urllib.error.URLError):
            if process.poll() is not None:
                raise RuntimeError('Verification process exited; inspect private server.log')
            time.sleep(0.5)
    else:
        raise RuntimeError('Verification health timed out')
    state['health'] = data
    save_private(STAGE / 'state.json', state)
    status, created, _ = http('/api/keys', {'name':'protocol-verification','max_concurrent':1}, admin=True)
    if status != 201 or not created.get('key'):
        raise RuntimeError('Could not create isolated verification key')
    state['key'] = created['key']
    status, second, _ = http('/api/keys', {'name':'protocol-isolation-verification','max_concurrent':1}, admin=True)
    if status != 201:
        raise RuntimeError('Could not create second verification key')
    state['second_key'] = second['key']
    save_private(STAGE / 'state.json', state)
    print(json.dumps({'setup':'ready','accounts':state['accounts'],'binary_sha256':state['binary_sha256'],'build':data.get('build')}, ensure_ascii=False), flush=True)


def text_of(data):
    parts = []
    for item in data.get('output', []):
        if isinstance(item, dict):
            for part in item.get('content', []):
                if isinstance(part, dict) and part.get('type') == 'output_text':
                    parts.append(part.get('text', ''))
    if data.get('output_text'):
        return data['output_text']
    return ''.join(parts)


def record(channel, scenario, status, passed, elapsed, **details):
    entry = dict(channel=channel, scenario=scenario, http=status, passed=passed, seconds=elapsed, **details)
    with open(STAGE / 'results.jsonl', 'a') as stream:
        stream.write(json.dumps(entry, ensure_ascii=False) + '\n')
    print(json.dumps(entry, ensure_ascii=False), flush=True)


def extra_probe(channel, model, cache=False):
    path = '/' + channel + '/v1/responses'
    common = {'model':model,'max_output_tokens':2048}
    if cache:
        for attempt in (1,2):
            body = dict(common,input='Reply exactly PONG.',stream=False,prompt_cache_key='isolated-cache-verification')
            status,data,elapsed = http(path,body)
            usage = data.get('usage') or {}
            cached = (usage.get('input_tokens_details') or {}).get('cached_tokens')
            record(channel,'prompt_cache_key_acceptance',status,status == 200 and 'PONG' in text_of(data),elapsed,model=model,attempt=attempt,cached_input_tokens=cached)
        return
    tools = []
    for name in ('probe_first','probe_second'):
        tools.append({'type':'function','name':name,'description':'Echo a value','parameters':{'type':'object','properties':{'value':{'type':'string'}},'required':['value'],'additionalProperties':False}})
    body = dict(common,input='Call probe_first with value FIRST and probe_second with value SECOND. Call both tools now, without text.',tools=tools,tool_choice='required',parallel_tool_calls=True,stream=True)
    status,data,elapsed = http(path,body)
    wire = data.get('_wire','')
    final = None
    for frame in wire.replace('\r\n','\n').split('\n\n'):
        for line in frame.splitlines():
            if line.startswith('data: '):
                try:
                    event = json.loads(line[6:])
                except ValueError:
                    continue
                if event.get('type') in ('response.completed','response.incomplete','response.failed'):
                    final = event.get('response')
    calls = [x for x in (final or {}).get('output',[]) if x.get('type') == 'function_call']
    arguments = {}
    try:
        arguments = {x['name']:json.loads(x['arguments']) for x in calls}
    except (ValueError,KeyError):
        pass
    passed = status == 200 and (final or {}).get('status') == 'completed' and len(calls) == 2 and len({x.get('call_id') for x in calls}) == 2 and arguments == {'probe_first':{'value':'FIRST'},'probe_second':{'value':'SECOND'}} and 'response.function_call_arguments.delta' in wire
    failure_message = ((final or {}).get('error') or {}).get('message')
    local_errors = ('tool identity changed','invalid or duplicate tool identity','invalid completed tool arguments','chat stream ended without finish_reason and [DONE]')
    record(channel,'two_tools_sse',status,passed,elapsed,model=model,call_count=len(calls),args_delta='response.function_call_arguments.delta' in wire,completed=(final or {}).get('status') == 'completed',response_status=(final or {}).get('status'),incomplete_details=(final or {}).get('incomplete_details'),error_code=((final or {}).get('error') or {}).get('code'),local_error=failure_message if failure_message in local_errors else None,max_output_tokens=2048)
    if passed:
        history = [{'type':'message','role':'user','content':body['input']}] + calls
        history += [{'type':'function_call_output','call_id':x['call_id'],'output':'RESULT_'+x['name']} for x in calls]
        history += [{'type':'message','role':'user','content':'Reply with both exact tool results, without calling tools.'}]
        status,data,elapsed = http(path,dict(common,input=history,stream=False))
        output = text_of(data)
        record(channel,'two_tool_results_followup',status,status == 200 and 'RESULT_probe_first' in output and 'RESULT_probe_second' in output,elapsed,model=model)


def probe(channel, model_override=None, schema_only=False):
    status, catalog, _ = http('/' + channel + '/v1/models')
    models = [m['id'] for m in catalog.get('data', [])]
    preference = {'workbuddy':['glm-5.3-flash','minimax-m2.5','claude-haiku'], 'cline':['gpt-5.6-luna','glm','gpt-5.4-mini'], 'grok':['grok-4.6','grok-4.5','grok-composer-2.5-fast']}[channel]
    model = next((m for wanted in preference for m in models if m == wanted), None)
    if not model:
        model = next((m for wanted in preference for m in models if wanted in m), models[0] if models else None)
    if model_override:
        if model_override not in models:
            record(channel,'catalog',400,False,0,reason='requested test model not visible')
            return
        model = model_override
    if not model:
        record(channel, 'catalog', status, False, 0, reason='no visible model')
        return
    record(channel, 'catalog', status, status == 200, 0, model=model, model_count=len(models))
    path = '/' + channel + '/v1/responses'
    common = {'model':model,'stream':False,'max_output_tokens':1024}
    if schema_only:
        schema = {'type':'json_schema','name':'constraint_probe','strict':True,'schema':{'type':'object','properties':{'marker':{'type':'string','enum':['SCHEMA_RIGHT']}},'required':['marker'],'additionalProperties':False}}
        status,data,elapsed = http(path,dict(common,input='Return this exact JSON: {"marker":"PROMPT_WRONG"}.',text={'format':schema}))
        try:
            structured = json.loads(text_of(data))
        except ValueError:
            structured = None
        record(channel,'strict_schema_conflicting_prompt',status,status == 200 and structured == {'marker':'SCHEMA_RIGHT'},elapsed,model=model,parsed_marker=structured.get('marker') if isinstance(structured,dict) else None,response_status=data.get('status'))
        return
    status, data, elapsed = http(path, dict(common, input='Reply with exactly PONG.'))
    good = status == 200 and data.get('status') == 'completed' and 'PONG' in text_of(data)
    record(channel, 'text', status, good, elapsed, response_status=data.get('status'), output_marker='PONG' in text_of(data), usage=data.get('usage'), error_type=(data.get('error') or {}).get('type'))
    if not good:
        return
    schema = {'type':'json_schema','name':'constraint_probe','strict':True,'schema':{'type':'object','properties':{'marker':{'type':'string','enum':['SCHEMA_RIGHT']}},'required':['marker'],'additionalProperties':False}}
    status, data, elapsed = http(path, dict(common, input='Return this exact JSON: {"marker":"PROMPT_WRONG"}.', text={'format':schema}))
    try:
        structured = json.loads(text_of(data))
    except ValueError:
        structured = None
    record(channel, 'strict_schema_conflicting_prompt', status, status == 200 and structured == {'marker':'SCHEMA_RIGHT'}, elapsed, parsed_marker=(structured or {}).get('marker') if isinstance(structured,dict) else None, response_status=data.get('status'))
    tools = [{'type':'function','name':'echo_probe','description':'Return the supplied value','parameters':{'type':'object','properties':{'value':{'type':'string'}},'required':['value'],'additionalProperties':False}}]
    initial = 'Call echo_probe once with value PING. Do not answer directly.'
    status, data, elapsed = http(path, dict(common, input=initial, tools=tools, tool_choice={'type':'function','name':'echo_probe'}))
    calls = [x for x in data.get('output', []) if isinstance(x,dict) and x.get('type') == 'function_call']
    valid = bool(calls) and all(x.get('call_id') and x.get('name') == 'echo_probe' for x in calls)
    try:
        valid = valid and all(json.loads(x.get('arguments','{}')).get('value') == 'PING' for x in calls)
    except ValueError:
        valid = False
    record(channel, 'function_call', status, status == 200 and valid, elapsed, call_count=len(calls), response_status=data.get('status'))
    if valid:
        history = [{'type':'message','role':'user','content':initial}] + calls
        history += [{'type':'function_call_output','call_id':x['call_id'],'output':'TOOL_RESULT_OK'} for x in calls]
        history += [{'type':'message','role':'user','content':'Reply exactly with the echo_probe tool result; do not call tools again.'}]
        status, data, elapsed = http(path, dict(common,input=history))
        record(channel,'tool_result_followup',status,status == 200 and 'TOOL_RESULT_OK' in text_of(data),elapsed,response_status=data.get('status'))
    compact_input = [{'type':'message','role':'user','content':'Remember the project marker VERIFY_JADE_47. We are editing main.go; the next task is adding tests.'},{'type':'message','role':'assistant','content':'I will preserve that marker and task.'}]
    status, data, elapsed = http(path + '/compact', dict(common,input=compact_input))
    items = [x for x in data.get('output',[]) if isinstance(x,dict) and x.get('type') == 'compaction' and x.get('encrypted_content')]
    record(channel,'compact',status,status == 200 and bool(items),elapsed,object=data.get('object'),compaction_count=len(items))
    if items:
        history = items + [{'type':'message','role':'user','content':'What is the exact saved project marker? Reply only with that marker.'}]
        body = dict(common,input=history)
        status, data, elapsed = http(path,body)
        record(channel,'compact_continuation',status,status == 200 and 'VERIFY_JADE_47' in text_of(data),elapsed,response_status=data.get('status'))
        if channel != 'grok':
            status, data, elapsed = http(path,body,key=load_state()['second_key'])
            record(channel,'compact_cross_key_rejected',status,status == 400,elapsed)
    status, data, elapsed = http(path,dict(common,input='Reply PONG.',stream=True))
    wire = data.get('_wire','')
    record(channel,'responses_sse',status,status == 200 and 'response.completed' in wire and 'response.output_text.delta' in wire and 'response.failed' not in wire,elapsed,completed='response.completed' in wire,failed='response.failed' in wire)
    status, data, elapsed = http('/'+channel+'/v1/messages', {'model':model,'messages':[{'role':'user','content':'Reply exactly PONG.'}],'stream':False,'max_tokens':1024})
    content = ''.join(p.get('text','') for p in data.get('content',[]) if isinstance(p,dict))
    record(channel,'claude_messages',status,status == 200 and 'PONG' in content,elapsed,stop_reason=data.get('stop_reason'))
    if channel != 'grok':
        for name,extra in [('unsupported_include',{'include':['reasoning.encrypted_content']}),('unsupported_hosted_tool',{'tools':[{'type':'web_search'}]})]:
            status,data,elapsed = http(path,dict(common,input='Reply PONG.',**extra))
            record(channel,name,status,status == 400,elapsed)


def cleanup():
    state = load_state()
    pid = state.get('pid')
    proc = Path('/proc/' + str(pid))
    if proc.exists():
        if (proc / 'exe').resolve() != (STAGE / 'orchids-server').resolve():
            raise RuntimeError('Refusing to stop a process outside this verification stage')
        os.kill(pid, signal.SIGTERM)
        for _ in range(20):
            if not proc.exists():
                break
            time.sleep(0.5)
    cfg = json.loads((STAGE / 'config.json').read_text())
    prefix = state['prefix']
    if not prefix.startswith('protocol-verify:') or prefix == cfg.get('source_prefix'):
        raise RuntimeError('Refusing unsafe Redis cleanup')
    redis = Redis(cfg)
    keys = redis.keys(prefix + '*')
    for key in keys:
        redis.command('DEL', key)
    for path in ('state.json','config.json','server.log'):
        (STAGE / path).unlink(missing_ok=True)
    print(json.dumps({'cleanup':'done','temporary_keys_deleted':len(keys),'process_stopped':not proc.exists()}),flush=True)


def restart():
    state = load_state()
    proc = Path('/proc/' + str(state['pid']))
    if proc.exists():
        if (proc / 'exe').resolve() != (STAGE / 'orchids-server').resolve():
            raise RuntimeError('Refusing to restart an unrelated process')
        os.kill(state['pid'],signal.SIGTERM)
        for _ in range(20):
            if not proc.exists():
                break
            time.sleep(0.5)
        else:
            raise RuntimeError('Temporary process did not stop')
    next_binary = STAGE / 'orchids-server.next'
    next_binary.chmod(0o700)
    next_binary.replace(STAGE / 'orchids-server')
    env = os.environ.copy()
    production_pid = subprocess.check_output(['systemctl','show','orchids-2api.service','-p','MainPID','--value']).decode().strip()
    for pair in Path('/proc/'+production_pid+'/environ').read_bytes().split(b'\0'):
        if pair.startswith(b'ORCHIDS_CREDENTIAL_ENCRYPTION_KEY='):
            env['ORCHIDS_CREDENTIAL_ENCRYPTION_KEY'] = pair.split(b'=',1)[1].decode()
    log = open(STAGE/'server.log','ab')
    process = subprocess.Popen([str(STAGE/'orchids-server'),'-config',str(STAGE/'config.json')],cwd=STAGE,env=env,stdout=log,stderr=log,start_new_session=True)
    state['pid'] = process.pid
    state['binary_sha256'] = hashlib.sha256((STAGE/'orchids-server').read_bytes()).hexdigest()
    save_private(STAGE/'state.json',state)
    for _ in range(30):
        try:
            status,data,_ = http('/health',timeout=2)
            if status == 200:
                state['health'] = data
                save_private(STAGE/'state.json',state)
                print(json.dumps({'restart':'ready','binary_sha256':state['binary_sha256'],'build':data.get('build')}),flush=True)
                return
        except (OSError,urllib.error.URLError):
            time.sleep(0.5)
    raise RuntimeError('Temporary restart health failed')


if __name__ == '__main__':
    action = sys.argv[1]
    if action == 'setup':
        setup()
    elif action in ('probe','schema'):
        for channel in sys.argv[2:] or CHANNELS:
            try:
                channel, _, model = channel.partition(':')
                probe(channel, model or None, schema_only=action == 'schema')
            except Exception as exc:
                record(channel,'probe_exception',0,False,0,exception_type=type(exc).__name__)
    elif action == 'inventory':
        for channel in CHANNELS:
            status,data,_ = http('/'+channel+'/v1/models',key=load_state()['second_key'])
            print(json.dumps({'channel':channel,'http':status,'models':[m['id'] for m in data.get('data',[])]}),flush=True)
    elif action in ('parallel','cache'):
        for target in sys.argv[2:]:
            channel,model = target.split(':',1)
            try:
                extra_probe(channel,model,cache=action == 'cache')
            except Exception as exc:
                record(channel,'probe_exception',0,False,0,model=model,exception_type=type(exc).__name__)
    elif action == 'cleanup':
        cleanup()
    elif action == 'restart':
        restart()
    else:
        raise SystemExit('Use setup, probe [channel ...], or cleanup')
