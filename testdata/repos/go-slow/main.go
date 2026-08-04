package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	ready := make(chan os.Signal, 1)
	signal.Notify(ready, syscall.SIGTERM, syscall.SIGINT)
	fmt.Println("ready")
	<-ready
}
