package main

import (
	"fmt"
	"os"

	"github.com/Bunn-Burrow/bunn-burrow/internal/network"
)

func main() {
	if len(os.Args) != 2 || os.Args[1] != "serve" {
		fmt.Fprintln(os.Stderr, "usage: bunn-network-helper serve")
		os.Exit(2)
	}
	if err := network.RunNetworkHelper(os.Stdin, os.Stdout); err != nil {
		os.Exit(1)
	}
}
