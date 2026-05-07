package disciplinetest

func OldName() string {
	return "x"
}

func Caller() string {
	return OldName()
}
