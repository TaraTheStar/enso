// SPDX-License-Identifier: AGPL-3.0-or-later

package workflow

import "runtime"

// runtimeNumGoroutine wraps runtime.NumGoroutine in a thin helper so
// the leak-check test can stub it in isolation if we ever want to. No
// behaviour change vs calling runtime.NumGoroutine directly.
func runtimeNumGoroutine() int { return runtime.NumGoroutine() }
