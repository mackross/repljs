package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/dop251/goja"
	"github.com/solidarity-ai/repl/engine"
	"github.com/solidarity-ai/repl/model"
)

type fetchDelegate struct{}

type fetchResult struct {
	Status int    `json:"status"`
	OK     bool   `json:"ok"`
	Body   string `json:"body"`
}

func (fetchDelegate) ConfigureRuntime(_ engine.SessionRuntimeContext, rt *goja.Runtime, host engine.HostFuncBuilder, state json.RawMessage) (json.RawMessage, error) {
	rawFetch := host.WrapAsync("fetch", model.ReplayReadonly, func(_ context.Context, params json.RawMessage) (json.RawMessage, error) {
		var url string
		if err := json.Unmarshal(params, &url); err != nil || url == "" {
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
		return json.Marshal(fetchResult{
			Status: resp.StatusCode,
			OK:     resp.StatusCode >= 200 && resp.StatusCode < 300,
			Body:   string(body),
		})
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
				resObj := rt.NewObject()
				_ = resObj.Set("status", payload["status"])
				_ = resObj.Set("ok", payload["ok"])
				body, _ := payload["body"].(string)
				_ = resObj.Set("text", func(goja.FunctionCall) goja.Value {
					p, r, _ := rt.NewPromise()
					_ = r(rt.ToValue(body))
					return rt.ToValue(p)
				})
				_ = resObj.Set("json", func(goja.FunctionCall) goja.Value {
					p, r, jReject := rt.NewPromise()
					var v any
					if err := json.Unmarshal([]byte(body), &v); err != nil {
						_ = jReject(rt.ToValue(fmt.Sprintf("fetch json parse failed: %v", err)))
						return rt.ToValue(p)
					}
					_ = r(rt.ToValue(v))
					return rt.ToValue(p)
				})
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
