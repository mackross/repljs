package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/dop251/goja"
	"github.com/solidarity-ai/repl/engine"
	"github.com/solidarity-ai/repl/jswire"
	"github.com/solidarity-ai/repl/model"
)

type fetchDelegate struct{}

func (fetchDelegate) ConfigureRuntime(_ engine.SessionRuntimeContext, rt *goja.Runtime, host engine.HostFuncBuilder, state json.RawMessage) (json.RawMessage, error) {
	fetchResultCtor, err := rt.RunString(`(() => {
		class FetchResult {
			constructor(status, ok, body) {
				this.status = status;
				this.ok = ok;
				Object.defineProperty(this, "__body", {
					value: body,
					writable: false,
					enumerable: false,
					configurable: false,
				});
			}

			async text() {
				return this.__body;
			}

			async json() {
				return JSON.parse(this.__body);
			}
		}

		return FetchResult;
	})()`)
	if err != nil {
		return nil, fmt.Errorf("install FetchResult: %w", err)
	}

	rawFetch := host.WrapAsync("fetch", model.ReplayReadonly, func(_ context.Context, params []byte) ([]byte, error) {
		decoded, err := jswire.DecodeGoja(goja.New(), params)
		if err != nil {
			return nil, fmt.Errorf("fetch: decode url: %w", err)
		}
		url, _ := decoded.Export().(string)
		if url == "" {
			return nil, fmt.Errorf("fetch: url required")
		}
		resp, err := http.Get(url)
		if err != nil {
			return nil, fmt.Errorf("fetch GET failed: %w", err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("fetch read body failed: %w", err)
		}
		return jswire.EncodeGoja(goja.New().ToValue(map[string]any{
			"status": resp.StatusCode,
			"ok":     resp.StatusCode >= 200 && resp.StatusCode < 300,
			"body":   string(body),
		}))
	})

	if err := rt.Set("fetch", func(call goja.FunctionCall) goja.Value {
		rawPromiseVal := rawFetch(call)
		rawPromiseObj := rawPromiseVal.ToObject(rt)
		then, ok := goja.AssertFunction(rawPromiseObj.Get("then"))
		if !ok {
			return rawPromiseVal
		}

		promise, resolve, reject := rt.NewPromise()
		_, err := then(rawPromiseObj,
			rt.ToValue(func(call goja.FunctionCall) goja.Value {
				payload, ok := call.Argument(0).Export().(map[string]any)
				if !ok {
					_ = reject(rt.ToValue("fetch: invalid response payload"))
					return goja.Undefined()
				}
				body, _ := payload["body"].(string)
				resObj, err := rt.New(
					fetchResultCtor,
					rt.ToValue(payload["status"]),
					rt.ToValue(payload["ok"]),
					rt.ToValue(body),
				)
				if err != nil {
					_ = reject(rt.ToValue(fmt.Sprintf("fetch: construct FetchResult: %v", err)))
					return goja.Undefined()
				}
				_ = resolve(resObj)
				return goja.Undefined()
			}),
			rt.ToValue(func(call goja.FunctionCall) goja.Value {
				_ = reject(call.Argument(0))
				return goja.Undefined()
			}),
		)
		if err != nil {
			_ = reject(rt.ToValue(fmt.Sprintf("fetch: then failed: %v", err)))
		}
		return rt.ToValue(promise)
	}); err != nil {
		return nil, fmt.Errorf("install fetch: %w", err)
	}
	if len(state) != 0 {
		return append(json.RawMessage(nil), state...), nil
	}
	return json.RawMessage(`{"kind":"basic-fetch","version":1}`), nil
}
