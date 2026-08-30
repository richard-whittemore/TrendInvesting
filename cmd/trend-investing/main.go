package main

import (
	"fmt"

	"github.com/richard-whittemore/TrendInvesting/internal/buildinfo"
)

func main() {
	fmt.Printf("trend-investing %s\n", buildinfo.Version)
}
