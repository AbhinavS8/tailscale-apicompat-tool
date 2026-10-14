//core logic

package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("gorelease: ")

	wd, err := os.Getwd()
	if err != nil {
		log.Fatal(err)
	}

	ctx := context.WithValue(context.Background(), "env", append(os.Environ(), "GO111MODULE=on"))
	success, err := runCheck(ctx, os.Stdout, wd, os.Args[1:])
	if err != nil {
		if _, ok := err.(*usageError); ok {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		} else {
			log.Fatal(err)
		}
	}
	if !success {
		os.Exit(1)
	}
}

func runCheck(ctx context.Context, w io.Writer, dir string, args []string) (success bool, err error) {

	fmt.Println(ctx, dir, args)
	return true, nil

}
