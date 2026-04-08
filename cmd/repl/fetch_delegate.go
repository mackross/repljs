package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/dop251/goja"
	"github.com/solidarity-ai/repl/engine"
)

type fetchDelegate struct{}

func (fetchDelegate) ConfigureRuntime(_ engine.SessionRuntimeContext, rt *goja.Runtime, state json.RawMessage) (json.RawMessage, error) {
	if err := rt.Set("fetch", func(call goja.FunctionCall) goja.Value {
		promise, resolve, reject := rt.NewPromise()
		if len(call.Arguments) < 1 || goja.IsUndefined(call.Arguments[0]) || goja.IsNull(call.Arguments[0]) {
			_ = reject(rt.ToValue("fetch: url required"))
			return rt.ToValue(promise)
		}
		url := call.Arguments[0].String()
		go func() {
			resp, err := http.Get(url)
			if err != nil {
				engine.RunOnRuntimeLoop(rt, func(vm *goja.Runtime) {
					_ = reject(vm.ToValue(fmt.Sprintf("fetch GET failed: %v", err)))
				})
				return
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				engine.RunOnRuntimeLoop(rt, func(vm *goja.Runtime) {
					_ = reject(vm.ToValue(fmt.Sprintf("fetch read body failed: %v", err)))
				})
				return
			}
			engine.RunOnRuntimeLoop(rt, func(vm *goja.Runtime) {
				resObj := vm.NewObject()
				_ = resObj.Set("status", resp.StatusCode)
				_ = resObj.Set("ok", resp.StatusCode >= 200 && resp.StatusCode < 300)
				_ = resObj.Set("text", func(goja.FunctionCall) goja.Value {
					p, r, _ := vm.NewPromise()
					_ = r(vm.ToValue(string(body)))
					return vm.ToValue(p)
				})
				_ = resObj.Set("json", func(goja.FunctionCall) goja.Value {
					p, r, jReject := vm.NewPromise()
					var v any
					if err := json.Unmarshal(body, &v); err != nil {
						_ = jReject(vm.ToValue(fmt.Sprintf("fetch json parse failed: %v", err)))
						return vm.ToValue(p)
					}
					_ = r(vm.ToValue(v))
					return vm.ToValue(p)
				})
				_ = resolve(resObj)
			})
		}()
		return rt.ToValue(promise)
	}); err != nil {
		return nil, fmt.Errorf("install fetch: %w", err)
	}
	if len(state) != 0 {
		return append(json.RawMessage(nil), state...), nil
	}
	return json.RawMessage(`{"kind":"basic-fetch","version":1}`), nil
}
