package main

import (
	"bytes"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

var (
	optLimit    int
	optAll      bool
	optShowLine bool
	optBefore   bool
	optOffset   = 0
	optAfter    bool
	optShowDate bool
	optCached   bool
	optHunks    string
	optShowHunk bool
)

type WantedHunks map[int]bool

func main() {
	flag.BoolVar(&optAll, "all", false, "Show all merge base tags.")
	flag.IntVar(&optLimit, "limit", 7, "Show only the given `number` of merge base tags. 0 is equivalent to -all.")
	flag.BoolVar(&optShowLine, "line", false, "Show the line numbers for each affected commit (will be shown\n\tregardless when there are no common commit).")
	flag.BoolVar(&optBefore, "B", false, "Use the commit immediately preceeding the changed line. Useful for one-liner\n\tchange when the surrounding commit is newer than the changed line's.")
	flag.BoolVar(&optAfter, "A", false, "Use the commit immediately following the changed line. Useful for one-liner\n\tchange when the surrounding commit is newer than the changed line's.")
	flag.BoolVar(&optShowDate, "date", false, "Show commit date")
	flag.BoolVar(&optCached, "cached", false, "Pass --cached option to git diff")
	flag.StringVar(&optHunks, "H", "", "Check the given hunks only (comma separated, first hunk is 1, from git diff -U0).")
	flag.BoolVar(&optShowHunk, "hunk", false, "Show hunk")
	flag.Parse()

	if optLimit == 0 {
		optAll = true
	}

	if optBefore {
		optOffset = -1
	} else if optAfter {
		optOffset = 1
	}

	args := flag.Args()
	if len(args) == 0 {
		bail("Usage: git check-diff <file>")
	}

	var hunks WantedHunks
	if optHunks != "" {
		if len(args) > 1 {
			bail("-H works only with one file")
		}
		hunks = WantedHunks{}
		for _, v := range strings.Split(optHunks, ",") {
			n, err := strconv.Atoi(v)
			if err != nil {
				bail("%s: %v", v, err)
			}
			hunks[n] = true
		}
	}

	tagsSeen := map[string]int{}
	for i, filename := range args {
		for _, tag := range checkDiff(filename, hunks) {
			tagsSeen[tag]++
		}
		if i > 0 && i < len(args)-1 {
			fmt.Println()
		}
	}

	var commonTags MergeBaseTags
	for tag, count := range tagsSeen {
		if count == len(args) {
			commonTags = append(commonTags, tag)
		}
	}
	if len(args) > 1 {
		fmt.Println()
		if len(commonTags) > 0 {
			sort.Sort(commonTags)
			fmt.Printf("COMMON TAG: %s\n", commonTags)
		} else {
			fmt.Printf("NO COMMON TAG\n")
		}
	}

}

type MergeBaseTags []string

func (m MergeBaseTags) Len() int           { return len(m) }
func (m MergeBaseTags) Swap(i, j int)      { m[i], m[j] = m[j], m[i] }
func (m MergeBaseTags) Less(i, j int) bool { return getTagNumber(m[i]) < getTagNumber(m[j]) }

func (m MergeBaseTags) String() string {
	b := &bytes.Buffer{}
	for i, tag := range m {
		fmt.Fprintf(b, "%s ", tag)
		if !optAll && optLimit > 0 && i+1 >= optLimit && i < len(m)-1 {
			fmt.Fprintf(b, "... %d more (use -all to show all)", len(m)-(i+1))
			break
		}
	}
	return b.String()
}

func getTagNumber(mbtag string) int {
	if !strings.HasPrefix(mbtag, "MERGE_BASE_") {
		panic(fmt.Sprintf("%s is not a MERGE_BASE tag", mbtag))
	}
	chunks := strings.Split(mbtag, "_")
	if len(chunks) != 3 {
		panic(fmt.Sprintf("%s do not match MERGE_BASE_N pattern", mbtag))
	}

	n, err := strconv.Atoi(chunks[2])
	if err != nil {
		panic(fmt.Sprintf("%s: %v", mbtag, err))
	}
	return n
}

var (
	HUNK_PREFIX = []byte{'@', '@', ' ', '-'}
	SPACE       = []byte{' '}
	COMMA       = []byte{','}
)

func checkDiff(file string, hunks WantedHunks) MergeBaseTags {
	var commonTags MergeBaseTags
	fmt.Printf("%s\n", file)
	blame := getBlame(file)
	commitsAffected := map[string]MergeBaseTags{}

	linesForCommit := map[string][]int{}
	gitDiffArgs := []string{"diff", "-U0"}
	if optCached {
		gitDiffArgs = append(gitDiffArgs, "--cached")
	}
	gitDiffArgs = append(gitDiffArgs, "--", file)

	buf, err := exec.Command("git", gitDiffArgs...).Output()
	if err != nil {
		bail("error: %v", err)
	}
	diff, err := NewDiff(bytes.NewReader(buf))
	if err != nil {
		bail("error: %v", err)
	}

	if hunks != nil {
		odiff := diff
		diff = Diff{}
		for i, hunk := range odiff.Hunks {
			if !hunks[i+1] {
				continue
			}
			diff.Added += hunk.Added.Count
			diff.Removed += hunk.Removed.Count
			diff.Hunks = append(diff.Hunks, hunk)
		}
	}

	fmt.Printf("    Lines: %d removed, %d added\n", diff.Removed, diff.Added)
	for _, hunk := range diff.Hunks {
		if optShowHunk {
			fmt.Printf("%s\n", hunk.diff)
		}
		if hunk.Removed.Count == 0 {
			// no lines removed, just new lines added

			// verify that new lines are added
			if hunk.Added.Count == 0 {
				bail("FIXME expecting added hunk count greater than 0 but got %d", hunk.Added.Count)
			}

			lnum := hunk.Removed.Start
			if lnum == 0 {
				lnum = 1
			}
			sha1 := blame.sha1(lnum)
			if len(sha1) == 0 {
				continue
			}
			commitsAffected[sha1] = nil
			linesForCommit[sha1] = append(linesForCommit[sha1], lnum)
		} else {
			from := hunk.Removed.Start
			count := hunk.Removed.Count
			if count > 1 {
				for lnum := from; lnum < from+count; lnum++ {
					lnum := lnum + optOffset
					if lnum > 0 && lnum < len(blame) {
						sha1 := blame.sha1(lnum)
						if len(sha1) == 0 {
							continue
						}
						commitsAffected[sha1] = nil
						linesForCommit[sha1] = append(linesForCommit[sha1], lnum)
					} else {
						fmt.Printf("DEBUG out of bound len(blame) = %d, lnum %d\n", len(blame), lnum)
					}
				}
			} else {
				lnum := from + optOffset
				sha1 := blame.sha1(lnum)
				if len(sha1) == 0 {
					continue
				}
				commitsAffected[sha1] = nil
				linesForCommit[sha1] = append(linesForCommit[sha1], lnum)
			}
		}
	}
	tagsSeen := map[string]int{}
	nCommits := len(commitsAffected)
	for sha1, _ := range commitsAffected {
		tags := findMergeBaseTags(sha1)
		commitsAffected[sha1] = tags
		for _, tag := range tags {
			tagsSeen[tag]++
		}
		//fmt.Printf("\t%s %s\n", sha1, getAffectedBranches(sha1))

	}

	hotTags := []string{}
	var tags MergeBaseTags
	for tag, count := range tagsSeen {
		if count == nCommits {
			tags = append(tags, tag)
		} else if count > 1 {
			hotTags = append(hotTags, tag)
		}
	}

	if len(tags) > 0 {
		// We have a common commit for all the affected commits
		fmt.Printf("    Commits affected:\n")
		// TODO when showing affected commits, sort them by their line numbers
		for sha1, _ := range commitsAffected {
			showCommit(sha1)
			if optShowLine {
				showLines(linesForCommit[sha1])
			}
		}
		fmt.Printf("    Common tag:\n")
		sort.Sort(tags)
		fmt.Printf("\t%s\n", tags)
		commonTags = tags
	} else {
		// print relevant tags for this sha1
		fmt.Printf("    No common tags found for all the affected commits.\n")
		for sha1, tags := range commitsAffected {
			showCommit(sha1)
			fmt.Printf("\t\t")
			sort.Sort(tags)
			tagsToShow := &bytes.Buffer{}
			for _, tag := range tags {
				if tagsSeen[tag] > 1 {
					fmt.Fprintf(tagsToShow, "%s ", tag)
				}
			}
			if tagsToShow.Len() > 0 {
				fmt.Printf("%s\n", tagsToShow)
			}
			showLines(linesForCommit[sha1])
		}
	}

	return commonTags
}

func showCommit(sha1 string) {
	fmt.Printf("\t%s", sha1)
	if optShowDate {
		fmt.Printf(" %s", getCommitDate(sha1))
	}
	fmt.Printf(" %s\n", getAffectedBranches(sha1))
}

func getCommitDate(ref string) time.Time {
	l := linesFrom("git", "show", "--no-patch", "--format=%at", ref)
	date := string(l[0])
	n, err := strconv.Atoi(date)
	if err != nil {
		log.Panicf("error parsing commit date %s: %v", date, err)
	}
	return time.Unix(int64(n), 0)
}

func showLines(lnums []int) {
	lines := &bytes.Buffer{}
	for _, lnum := range lnums {
		fmt.Fprintf(lines, "%d ", lnum)
	}
	if lines.Len() > 0 {
		fmt.Printf("\tlines: %s\n", lines)
	}
}
func getAffectedBranches(sha1 string) string {
	var branches []string
	for _, b := range linesFrom("git", "branch", "--list", "--all", "--contains", sha1, "origin/release-*", "origin/develop") {
		b = bytes.TrimLeft(b, " *")
		branch := strings.TrimPrefix(string(b), "remotes/")
		switch {
		case branch == "origin/develop":
			branches = append(branches, branch)
		case strings.HasPrefix(branch, "origin/release-"):
			branches = append(branches, branch)
		}
	}
	return "(" + strings.Join(branches, ", ") + ")"
}

func findMergeBaseTags(sha1 string) []string {
	var tags []string
	for _, line := range linesFrom("git", "tag", "--contains", sha1, "-l", "MERGE_BASE_*") {
		if len(line) > 0 {
			tags = append(tags, string(line))
		}
	}
	return tags
}

func asInt(buf []byte) int {
	n, err := strconv.Atoi(string(buf))
	if err != nil {
		bail("%s: %v", buf, err)
	}
	return n
}

type LineBlame []byte

func (lb LineBlame) sha1() string {
	i := bytes.Index(lb, []byte{' '})
	if i < 0 {
		return ""
	}
	return string(lb[0:i])
}

type Blame []LineBlame

func getBlame(file string) Blame {
	blame := Blame{[]byte("NIL")}
	for _, line := range linesFrom("git", "blame", "-l", "--root", "-r", "HEAD", file) {
		lblame := LineBlame(line)
		blame = append(blame, lblame)
	}
	return blame
}

func (b Blame) sha1(lnum int) string {
	return b[lnum].sha1()
}

func linesFrom(command string, arg ...string) [][]byte {
	return bytes.Split(run(command, arg...), []byte{'\n'})
}

func run(name string, arg ...string) []byte {
	buf, err := exec.Command(name, arg...).Output()
	if err != nil {
		bail("%v", err)
	}
	return buf
}

func bail(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
