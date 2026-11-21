#!/usr/bin/env python3
"""Offline C-ABI smoke test. Run with a temporary working directory, never CPA's cwd."""
import argparse
import base64
import collections
import ctypes
import json
from pathlib import Path
import re
import tempfile
import yaml


class Buffer(ctypes.Structure):
    _fields_ = [("ptr", ctypes.c_void_p), ("len", ctypes.c_size_t)]


class API(ctypes.Structure):
    _fields_ = [("abi_version", ctypes.c_uint32), ("first", ctypes.c_void_p),
                ("second", ctypes.c_void_p), ("third", ctypes.c_void_p)]


def encoded(value):
    return base64.b64encode(value).decode()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--library", required=True, type=Path)
    parser.add_argument("--policy", required=True, type=Path)
    parser.add_argument("--expected-version", required=True)
    parser.add_argument("--logs", type=Path)
    args = parser.parse_args()
    if Path.cwd().joinpath("config.yaml").exists() or Path.cwd().joinpath("data").exists():
        parser.error("use a temporary cwd without CPA config/data")
    policy = yaml.safe_load(args.policy.read_text())
    library = ctypes.CDLL(str(args.library.resolve()))
    host, plugin = API(), API()
    host.abi_version = 1
    initialize = library.cliproxy_plugin_init
    initialize.argtypes = [ctypes.POINTER(API), ctypes.POINTER(API)]
    initialize.restype = ctypes.c_int
    assert initialize(ctypes.byref(host), ctypes.byref(plugin)) == 0
    assert plugin.abi_version == 1
    invoke = ctypes.CFUNCTYPE(ctypes.c_int, ctypes.c_char_p, ctypes.c_void_p,
                             ctypes.c_size_t, ctypes.POINTER(Buffer))(plugin.first)
    free = ctypes.CFUNCTYPE(None, ctypes.c_void_p, ctypes.c_size_t)(plugin.second)

    def call(method, payload):
        request = json.dumps(payload, ensure_ascii=False).encode()
        memory = ctypes.create_string_buffer(request)
        response = Buffer()
        status = invoke(method.encode(), memory, len(request), ctypes.byref(response))
        try:
            assert status == 0, f"C ABI call failed: {method}"
            envelope = json.loads(ctypes.string_at(response.ptr, response.len))
            assert envelope.get("ok"), f"plugin rejected {method}: {envelope.get('error')}"
            return envelope["result"]
        finally:
            if response.ptr:
                free(response.ptr, response.len)

    with tempfile.TemporaryDirectory(prefix="guard-abi-smoke-") as directory:
        config = dict(policy, state_file=directory+"/guard.json", probe_state_file=directory+"/probe.json",
                      probe_min_samples=2, debug_log_enabled=False)
        config["session_routing"]["state_file"] = directory+"/routes.json"
        registration = call("plugin.register", {"config_yaml": encoded(yaml.safe_dump(config).encode())})
        assert registration["metadata"]["Version"] == args.expected_version
        assert registration["capabilities"]["scheduler"]
        assert registration["capabilities"]["scheduler_across_priorities"]
        routes = config["session_routing"]["categories"]
        identities = {name: routes[name]["preferred_auth_ids"][0] for name in ("general", "benchmark", "quality")}
        pool_id = "offline-probe-account"
        candidates = [dict(ID=id, Provider="codex", Priority=4 if name=="quality" else 7, Status="active")
                      for name,id in identities.items()]
        candidates.append(dict(ID=pool_id, Provider="codex", Priority=5, Status="active"))

        def before(id, body, model="gpt-6-astra", ua="release-smoke", session=None, caller="offline-release-caller"):
            headers = {"User-Agent": [ua]}
            if session:
                headers["Session-Id"] = [session]
            return call("request.intercept_before", dict(RequestID=id, Model=model, Stream=bool(body.get("stream")), CallerScope=caller,
                        Headers=headers, Body=encoded(json.dumps(body, ensure_ascii=False).encode())))

        def pick(response, expected, model="gpt-6-astra", available=None):
            selected = call("scheduler.pick", dict(Model=model, Options=dict(Headers=response.get("Headers", {}),
                            Metadata={"caller_scope":"offline-release-caller"}), Candidates=available or candidates))
            assert selected.get("Handled") and selected.get("AuthID")==expected, f"unexpected selection: {selected}"
            marker = response["Headers"]["X-Cpa-Internal-Session-Route"][0]
            after = call("request.intercept_after", dict(RequestID=marker, Headers=response["Headers"],
                         Metadata={"selected_auth_id":expected}))
            assert "X-Cpa-Internal-Session-Route" in after.get("ClearHeaders", [])

        pick(before("ordinary", {"input":"A normal short question"}), identities["general"])
        pick(before("svg", {"input":"Generate a self-contained, valid SVG of a bicycle. supported native SVG animation elements. Output requirements: return SVG."}), identities["benchmark"])
        pick(before("candy", {"input":"题目 在一个黑色的袋子里放有三种口味的糖果 苹果味 桃子味 西瓜味 五角星形 输出要求：只输出最终答案"}, model="gpt-6.1-sol"), identities["benchmark"], "gpt-6.1-sol")
        pick(before("integer", {"input":"生成一组不承载语义的整数选择。1 到 355（含端点）本任务必须由当前语言模型直接完成 禁止调用或借助任何工具 直接从第一个取值开始输出"}, model="gpt-6.1-sol", ua="Codex Desktop/0.147.0"), pool_id, "gpt-6.1-sol")
        pick(before("greeting", {"input":"say hi"}, ua="Go-http-client/2.0"), pool_id)
        pick(before("quality", {"input":"x"*1000,"reasoning":{"effort":"medium"}}, session="quality-session"), identities["quality"])
        pick(before("followup", {"input":"continue"}, session="quality-session"), identities["quality"])
        protected = next(c for c in candidates if c["ID"]==identities["quality"])
        pick(before("fallback", {"input":"short"}), pool_id, available=[protected,candidates[-1]])
        rejected = call("scheduler.pick", dict(Model="gpt-6-astra", Options=dict(Headers={}, Metadata={"caller_scope":"offline-release-caller"}), Candidates=[protected]))
        assert rejected.get("Handled") and rejected.get("Reject")

        if args.logs:
            counts, skipped = collections.Counter(), collections.Counter()
            for index,path in enumerate(sorted(args.logs.glob("*responses*.log"))):
                try:
                    initial = path.stat()
                    text = path.read_text(errors="replace")
                    current = path.stat()
                    if (initial.st_size,initial.st_mtime_ns)!=(current.st_size,current.st_mtime_ns):
                        skipped["changed"]+=1
                        continue
                    info = text.split("=== HEADERS ===",1)[0]
                    url = re.search(r"^URL: (.+)$",info,re.M)
                    if not url or url.group(1).split("?",1)[0]!="/v1/responses":
                        continue
                    body,_ = json.JSONDecoder().raw_decode(text.split("=== REQUEST BODY ===",1)[1].lstrip())
                    model = body.get("model","")
                    if not model.startswith("gpt-"):
                        continue
                    raw_headers=text.split("=== HEADERS ===",1)[1].split("=== REQUEST BODY ===",1)[0]
                    ua=re.search(r"(?mi)^User-Agent:\s*(.+)$",raw_headers)
                    replay_caller="offline-replay-"+str(index)
                    result=before("replay-"+str(index),body,model,ua.group(1) if ua else "",caller=replay_caller)
                    if result.get("Terminate"):
                        counts["local_or_cache" if result.get("StatusCode")==200 else "validation_reject"]+=1
                    else:
                        # Pending keys are opaque; inspect temporary persisted categories only.
                        marker=result.get("Headers",{}).get("X-Cpa-Internal-Session-Route")
                        if marker:
                            picked=call("scheduler.pick",dict(Model=model,Options=dict(Headers=result["Headers"],Metadata={"caller_scope":replay_caller}),Candidates=candidates))
                            target=picked.get("AuthID")
                            counts[next((name for name,id in identities.items() if id==target),"probe_or_fallback")]+=1
                        else:
                            counts["host_deferral"]+=1
                except FileNotFoundError:
                    skipped["disappeared"]+=1
                except (OSError,ValueError,IndexError,AttributeError,TypeError):
                    skipped["unreadable_or_invalid"]+=1
            print("Historical offline C-ABI replay (independent fresh bindings; synthetic availability):",json.dumps(dict(counts=counts,skipped=skipped),ensure_ascii=False))

        arithmetic = dict(model="gpt-5.6-sol", stream=True, max_output_tokens=50,
            instructions="You are a provider health-check endpoint. Answer the arithmetic challenge exactly and briefly.",
            input="Calculate and respond with ONLY the number, nothing else.\n\nQ: 3 + 5 = ?\nA: 8\n\nQ: 12 - 7 = ?\nA: 5\n\nQ: 21 + 30 = ?\nA:")
        response = before("local", arithmetic, model="gpt-5.6-sol")
        assert response.get("Terminate") and response.get("StatusCode")==200
        assert '"delta":"51"' in base64.b64decode(response["ResponseBody"]).decode()
        cached_body = dict(model="gpt-6-astra",input="offline cache smoke",stream=False)
        sample = dict(id="resp_offline",object="response",status="completed",
            usage=dict(input_tokens=1,output_tokens=1,total_tokens=2),
            output=[dict(id="msg_offline",type="message",role="assistant",status="completed",content=[dict(type="output_text",text="cached",annotations=[])])])
        for index in range(2):
            call("response.intercept_after",dict(RequestID="sample-"+str(index),Model="gpt-6-astra",StatusCode=200,
                 OriginalRequest=encoded(json.dumps(cached_body).encode()),ResponseBody=encoded(json.dumps(sample).encode())))
        state_before=Path(directory+"/routes.json").read_bytes()
        response=before("cache-hit",cached_body)
        assert response.get("Terminate") and response.get("StatusCode")==200
        assert Path(directory+"/routes.json").read_bytes()==state_before
        print("PASS: registration/version, 8 routing selections, reserved rejection, header stripping, arithmetic and cache short-circuit")


if __name__=="__main__":
    main()
