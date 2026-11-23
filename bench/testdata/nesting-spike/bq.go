package main

import (
	"fmt"
	"strings"
)

// backquotes nests `...` n levels deep, unclosed: level k is opened by a
// backquote preceded by 2^(k-1)-1 backslashes.
func backquotes() {
	for n := 1; n <= 21; n++ {
		var b strings.Builder
		b.WriteString("echo ")
		for k := 1; k <= n; k++ {
			b.WriteString(strings.Repeat("\\", 1<<(k-1)-1) + "`echo ")
		}
		src := b.String()
		r := parse([]byte(src), 1024, false)
		fmt.Printf("backquote levels=%2d bytes=%8d max frames=%d\n", n, len(src), r.maxF)
	}
}
