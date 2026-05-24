package main

import "aitools/tests/testprojects/go/pkg1"
import "aitools/tests/testprojects/go/pkg2"

func main() {
	var e pkg1.Equaler = pkg2.MyInt(1)
	e.Equal(2)
}
