package diff

// Sample before/after for the demo hunk — shared by the headless --diff mode
// and the TUI /diff command (one source of truth, matcha: reuse before write).
const (
	sampleBefore = `package main

func main() {
    fmt.Println("hello")
}
`
	sampleAfter = `package main

func main() {
    name := "brocode"
    fmt.Println("hello", name)
}
`
)

// Sample returns the demo Myers unified diff used by --diff and /diff.
func Sample() string {
	return Unified("main.go", "main.go", sampleBefore, sampleAfter)
}
