package main

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

func search() {
	toks := []string{"(", ")", "{", "}", "[", "]", "$", "\"", "'", "`", "<", ">", "!", "?", ":", "=", "-", "+", "*", "&", "|", ";", " ", "\n", "a", "1", "#", "\\", ",", "~", "@", "/", "%", "^", ".",
		"$(", "${", "$((", "((", "[[", "$[", "<(", "if ", "then ", "{ ", "a=", "a[", "${a[", "${a:", "${a/", "$'", "$\"", "function ", "a() ", "case a in ", "<<", "while ", "do ", "for ((", "select a in ", "let ", "time ", "coproc ", "declare ", "a=(", "+(", "@(", "!("}
	pres := []string{"", "echo ", "echo $((", "[[ ", "x=", "echo ${x:", "a[", "let ", "echo \"", "case a in ", "for ((", "declare ", "a=("}
	type res struct {
		pre, unit string
		fpb       float64
	}
	var out []res
	const reps = 150
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 16)
	for _, pre := range pres {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			base := parse([]byte(pre), 1024, false).maxF
			for _, a := range toks {
				for _, b := range append([]string{""}, toks...) {
					for _, c := range append([]string{""}, toks...) {
						if b == "" && c != "" {
							continue
						}
						u := a + b + c
						r := parse([]byte(pre+strings.Repeat(u, reps)), 1024, false)
						f := float64(r.maxF-base) / float64(reps*len(u))
						if f > 4 {
							mu.Lock()
							out = append(out, res{pre, u, f})
							mu.Unlock()
						}
					}
				}
			}
		}()
	}
	wg.Wait()
	sort.Slice(out, func(i, j int) bool { return out[i].fpb > out[j].fpb })
	seen := 0
	for _, r := range out {
		fmt.Printf("%6.2f frames/byte  pre=%q unit=%q\n", r.fpb, r.pre, r.unit)
		if seen++; seen > 40 {
			break
		}
	}
	fmt.Println("candidates over 4 frames/byte:", len(out))
}
