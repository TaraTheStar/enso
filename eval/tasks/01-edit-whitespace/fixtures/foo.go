package edittest

// OldName returns a fixed greeting.
//
// Note: the blank line above this comment and below this comment are
// deliberate; do not collapse them.

func OldName() string {
	return "hello"
}

func Caller() string {
	return OldName() + "!"
}
