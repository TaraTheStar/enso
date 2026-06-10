
## Summary
This PR performs a codebase-wide refactoring to modernize the use of `any` instead of `interface{}` and corrects technical inaccuracies in documentation and internal protocols.

## Key Changes

### 🧹 Refactoring: `interface{}` $	o$ `any`
Modernized the codebase by replacing `interface{}` with the `any` alias. This change affects:
- Tool parameters and return types.
- Internal agent and backend data structures.
- Wire protocols and JSON marshaling logic.
- Test suites across the entire repository.

### 📖 Documentation Accuracy
- Updated Gemini/Vertex documentation to remove the deprecated `civic_integrity` safety category.
- Clarified the purpose of various JSON events in the advanced documentation.

### 🛠️ Technical Refinements
- **Bus Protocol:** Fixed a gap in `eventTypeString` for `EventNotice`, ensuring host-local notices are correctly identified in logs.
- **Concurrency:** Improved documentation and naming (e.g., `appendMessageLocked`) to make locking requirements and concurrency invariants explicit.
- **Debug Logging:** Wrapped LLM-specific debug logs with `debugEnabled` checks to reduce runtime overhead when debugging is inactive.

## Verification Performed
- `go test ./...` passed.
- `go vet ./...` passed.
- `make check` (build/format/test) passed.
