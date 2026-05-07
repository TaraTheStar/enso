package main

import (
	"flag"
	"fmt"
	"os"
)

type Config struct {
	Greeting string
}

func Run(cfg Config, w *os.File) {
	fmt.Fprintln(w, cfg.Greeting)
}

func main() {
	greeting := flag.String("greeting", "hello", "what to print")
	flag.Parse()
	Run(Config{Greeting: *greeting}, os.Stdout)
}
