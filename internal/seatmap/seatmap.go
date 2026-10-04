// Package seatmap works out which seats sit next to each other, to suggest
// alternatives when a group's requested seats are not all free.
package seatmap

import (
	"regexp"
	"sort"
	"strconv"
)

var seatName = regexp.MustCompile(`^(.*?)(\d+)$`)

type seat struct {
	name string
	row  string
	num  int
	ok   bool // name parsed as row + number
}

// Runs splits seats into blocks of side-by-side seats: same row, consecutive
// numbers ("A3", "A4", "A5"). A seat whose name has no trailing number stands
// alone. Runs are ordered by row, then by seat number.
func Runs(names []string) [][]string {
	seats := make([]seat, 0, len(names))
	for _, n := range names {
		s := seat{name: n, row: n}
		if m := seatName.FindStringSubmatch(n); m != nil {
			if num, err := strconv.Atoi(m[2]); err == nil {
				s.row, s.num, s.ok = m[1], num, true
			}
		}
		seats = append(seats, s)
	}
	sort.SliceStable(seats, func(i, j int) bool {
		if seats[i].row != seats[j].row {
			return seats[i].row < seats[j].row
		}
		return seats[i].num < seats[j].num
	})

	var runs [][]string
	for i, s := range seats {
		if i > 0 {
			prev := seats[i-1]
			if s.ok && prev.ok && s.row == prev.row && s.num == prev.num+1 {
				runs[len(runs)-1] = append(runs[len(runs)-1], s.name)
				continue
			}
		}
		runs = append(runs, []string{s.name})
	}
	return runs
}

// Suggest picks n of the available seats, keeping the group as close together as possible:
//   - a block of exactly n side-by-side seats, if one exists;
//   - else the first n seats of the longest block, if it is long enough;
//   - else the whole longest block, with the rest of the group placed the same
//     way among the remaining blocks.
//
// It returns fewer than n seats only when fewer than n are available.
func Suggest(available []string, n int) []string {
	return pick(Runs(available), n)
}

func pick(runs [][]string, n int) []string {
	if n <= 0 || len(runs) == 0 {
		return []string{}
	}
	longest := 0
	for i, r := range runs {
		if len(r) == n {
			return r
		}
		if len(r) > len(runs[longest]) {
			longest = i
		}
	}
	if len(runs[longest]) >= n {
		return runs[longest][:n]
	}
	rest := append(append([][]string{}, runs[:longest]...), runs[longest+1:]...)
	return append(append([]string{}, runs[longest]...), pick(rest, n-len(runs[longest]))...)
}
