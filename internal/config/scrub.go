// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import "reflect"

// The `worker:"deny"` struct tag marks a field that must NEVER cross
// the Backend seam into the worker, regardless of backend. It is scoped
// deliberately narrowly: it is for secrets the worker has no legitimate
// use for because the host already does that job for it. Today that is
// exactly the provider Endpoint + APIKey — inference is host-proxied
// (the worker dials no model), so the worker never needs either, and
// keeping them out enforces the credential-scrub invariant at the type
// level.
//
// It is NOT a blanket "this is a secret" tag. Other secrets the worker
// genuinely uses to function (SearXNG api_key, MCP auth headers/env)
// are intentionally untagged: under LocalBackend there is no isolation
// boundary and scrubbing them would regress real features for zero
// security gain; under PodmanBackend they are withheld structurally
// (worker env starts empty; scoped grants arrive via the
// tier-3 broker), not by mangling the config. Adding a new
// host-proxied-only secret field is a one-line tag; scrubSecrets then
// covers it automatically and config_test asserts the invariant holds.
const denyTag = "worker"
const denyVal = "deny"

// scrubSecrets recursively zeroes every field reachable from v that
// carries `worker:"deny"`. It walks structs, pointers, slices/arrays
// and maps; map element structs are rebuilt because Go map values are
// not addressable. v must be addressable (call on a fresh deep copy).
func scrubSecrets(v reflect.Value) {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			scrubSecrets(v.Elem())
		}
	case reflect.Struct:
		t := v.Type()
		for i := 0; i < v.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			if tag, ok := f.Tag.Lookup(denyTag); ok && tag == denyVal {
				fv := v.Field(i)
				if fv.CanSet() {
					fv.Set(reflect.Zero(fv.Type()))
				}
				continue // whole field denied; no need to descend
			}
			scrubSecrets(v.Field(i))
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			scrubSecrets(v.Index(i))
		}
	case reflect.Map:
		for _, k := range v.MapKeys() {
			ev := v.MapIndex(k)
			// Copy into an addressable temp, scrub, write back.
			tmp := reflect.New(ev.Type()).Elem()
			tmp.Set(ev)
			scrubSecrets(tmp)
			v.SetMapIndex(k, tmp)
		}
	}
}
