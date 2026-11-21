// Command dealgen prints the deterministic deal for a seed, so e2e scripts
// (South moves + the fake agent's North script) can be written by hand.
package main

import (
	"flag"
	"fmt"
	"os"

	"euchre-demo/internal/euchre"
)

func main() {
	seed := flag.Int64("seed", 1, "shuffle seed")
	dealer := flag.String("dealer", "south", "first dealer")
	flag.Parse()
	d, err := euchre.ParseSeat(*dealer)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	g := euchre.NewGame(*seed, d)
	g.StartHand()
	for _, s := range []euchre.Seat{euchre.South, euchre.West, euchre.North, euchre.East} {
		fmt.Printf("%-6s %s\n", s, euchre.SortHand(g.Hand(s), euchre.Clubs))
	}
	fmt.Printf("top    %s\n", g.TopCard())
}
