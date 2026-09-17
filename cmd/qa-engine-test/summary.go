package main

import "fmt"

func printSummary() {
	pass, fail := 0, 0
	for _, r := range results {
		if r.pass {
			pass++
		} else {
			fail++
		}
	}
	fmt.Printf("\n%d/%d checks passed\n", pass, pass+fail)
	if fail > 0 {
		fmt.Println("\nFAILURES:")
		for _, r := range results {
			if !r.pass {
				fmt.Printf("  ✗ %s — %s\n", r.name, r.note)
			}
		}
	}
}
