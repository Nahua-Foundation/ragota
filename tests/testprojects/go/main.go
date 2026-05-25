package main

import "ragota/testprojects/go/pkg1"
import "ragota/testprojects/go/pkg2"

func main() {
	var e pkg1.Equaler = pkg2.MyInt(1)
	e.Equal(2)
}
