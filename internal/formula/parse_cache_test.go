package formula

import (
	"fmt"
	"sync"
	"testing"
)

// Evaluations share a formula's parsed tree (2026-11-18): the same text
// evaluated at the same time with different values, on many goroutines,
// answers each its own (run with -race: nothing writes to a shared tree); a
// text that does not parse answers its error every time; and a cache that
// fills up empties and goes on answering.
func TestEvaluationsShareAParsedFormula(t *testing.T) {
	const text = `IF(revenue > 100, revenue * rate, ROUND(revenue / 3, 2)) + LEN("x")`
	// want is the formula parsed afresh for this one evaluation, as before
	// the cache.
	want := func(revenue, rate float64) float64 {
		node, err := parse(text)
		if err != nil {
			t.Error(err) // never t.Fatal: want runs on the goroutines below
		}
		ctx := &EvalContext{Vars: upperKeys(map[string]Value{"revenue": NumberVal(revenue), "rate": NumberVal(rate)})}
		return ctx.safeEval(node).num
	}

	var wg sync.WaitGroup
	errs := make(chan string, 64)
	for g := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 200 {
				revenue, rate := float64(g*20+i), 0.5+float64(g)/10
				v, err := EvalWithContext(text, &EvalContext{Vars: map[string]Value{"revenue": NumberVal(revenue), "RATE": NumberVal(rate)}})
				if err != nil || v.num != want(revenue, rate) {
					errs <- fmt.Sprintf("revenue %v rate %v: %v %v, want %v", revenue, rate, v.num, err, want(revenue, rate))
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}

	for range 2 {
		if _, err := Eval("revenue *", map[string]Value{"revenue": NumberVal(1)}); err == nil {
			t.Error("a formula that does not parse evaluated without an error")
		}
	}

	for i := range maxParsed + 10 {
		v, err := Eval(fmt.Sprintf("x + %d", i), map[string]Value{"x": NumberVal(1)})
		if err != nil || v.num != float64(1+i) {
			t.Fatalf("x + %d = %v %v after %d formulas", i, v.num, err, i)
		}
	}
	if n := parsedCount.Load(); n > maxParsed {
		t.Errorf("the cache holds %d formulas, want at most %d", n, maxParsed)
	}
}
