//go:build ignore

// Command json prints fields of the objects in one array of the JSON document
// on stdin, an object a line. Usage: json items id path
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

func main() {
	var doc map[string][]map[string]any
	if err := json.NewDecoder(os.Stdin).Decode(&doc); err != nil {
		panic(err)
	}
	for _, it := range doc[os.Args[1]] {
		var out []string
		for _, f := range os.Args[2:] {
			out = append(out, fmt.Sprint(it[f]))
		}
		fmt.Println(strings.Join(out, " "))
	}
}
