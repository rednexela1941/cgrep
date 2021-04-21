package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/logrusorgru/aurora"
)

const (
	fileLimit      uint  = 1024
	allocBufferLen uint  = 64
	longFileLim    int64 = 5 * 1024 * 1024
)

var colors aurora.Aurora

var (
	file    = flag.String("f", "", "file path regular expression (including extension)")
	long    = flag.Bool("long", false, "search long files (>5mb)")
	noColor = flag.Bool("no-color", false, "disable colored output")
	color   = flag.Bool("color", false, "enable colored output")
	dir     = flag.String("dir", "", "starting directory path")
	help    = flag.Bool("h", false, "help")
)

var (
	whiteSpace   = regexp.MustCompile("[\\s]+")
	leadingSpace = regexp.MustCompile("^[\\s]+")
	ignorePath   = regexp.MustCompile("(.git|node_modules)$")
	tooManyOpen  = regexp.MustCompile("too many open files")
)

func main() {
	var err error
	var fprx *regexp.Regexp
	var rx *regexp.Regexp
	plock := new(sync.Mutex)
	wg := new(sync.WaitGroup)

	flag.Parse()
	outStat, err := os.Stdout.Stat()
	if err != nil {
		log.Fatal(err)
	}

	colors = aurora.NewAurora((outStat.Mode()&os.ModeNamedPipe == 0 && !*noColor) || *color)

	if *help {
		flag.PrintDefaults()
		return
	}

	in, err := os.Stdin.Stat()
	if err != nil {
		log.Fatal(err)
	}
	isPipe := in.Mode()&os.ModeNamedPipe != 0

	if len(*file) != 0 {
		fprx, err = regexp.Compile("(?i)" + *file)
		if err != nil {
			log.Fatalf("invalid regex %s", err.Error())
		}
	}

	args := flag.Args()
	if len(args) == 0 {
		if fprx == nil || isPipe {
			log.Fatal("no arguments provided")
		}
	} else {
		rx, err = regexp.Compile("(?i)" + args[0])
		if err != nil {
			log.Fatalf("invalid regex %s", err.Error())
		}
	}

	if isPipe {
		grepReader("STDIN", os.Stdin, rx, plock)
		return
	}

	root := *dir
	if len(root) == 0 {
		root, err = os.Getwd()
		if err != nil {
			log.Fatal(err)
		}
	}

	sem := make(chan struct{}, fileLimit)
	wg.Add(1)
	handleGrep(root, rx, fprx, wg, plock, sem)
	wg.Wait()
}

func handleGrep(root string, rx, fprx *regexp.Regexp, wg *sync.WaitGroup, plock *sync.Mutex, sem chan struct{}) error {
	defer wg.Done()
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			log.Fatal(err)
		}
		if info.IsDir() && path != root {
			if ignorePath.MatchString(path) {
				return filepath.SkipDir
			}
			wg.Add(1)
			go handleGrep(path, rx, fprx, wg, plock, sem)
			return filepath.SkipDir
		}
		if info.Mode().IsRegular() {
			if !*long && info.Size() > longFileLim && (fprx == nil || fprx.MatchString(path)) {
				fmt.Printf("skipping large file %s\n", path)
				return nil
			}
			wg.Add(1)
			go searchFile(path, rx, fprx, wg, plock, sem)
		}
		return nil
	})
	return err
}

func grepReader(path string, reader io.Reader, rx *regexp.Regexp, plock *sync.Mutex) {
	r := bufio.NewReader(reader)
	linenum := 0
	lines := make([]string, 0, allocBufferLen)

	for {
		l, err := r.ReadBytes('\n')
		if err != nil {
			if err != io.EOF {
				log.Println(err)
			}
			break
		}
		linenum++
		if rx.Match(l) {
			l = whiteSpace.ReplaceAll(leadingSpace.ReplaceAll(l, []byte("")), []byte(" "))
			ms := rx.FindAllIndex(l, -1)
			lm := len(ms)
			ll := len(l)
			oleft := 0
			lastnl := 0

			for i, m := range ms {
				left, right := m[0], m[1]
				if left > oleft+80 {
					oleft = left - 10
				}
				rightLim := oleft + 80
				if rightLim < right {
					rightLim = right
				}
				if rightLim > ll {
					rightLim = ll
				}
				if i+1 < lm {
					nextl := ms[i+1][0]
					if nextl < rightLim {
						rightLim = nextl
					}
				}
				b := formatLine(l[oleft:rightLim], left-oleft, right-oleft, linenum, i)
				oleft = rightLim
				if oleft > lastnl+80 || i+1 == lm {
					b += "\n"
				}
				lines = append(lines, b)
			}
		}
	}
	if ln := len(lines); ln > 0 {
		plock.Lock()
		defer plock.Unlock()
		fmt.Print(formatHeader(path, ln))
		for _, l := range lines {
			fmt.Print(l)
		}
	}
	lines = nil
}

func searchFile(path string, rx, fprx *regexp.Regexp, wg *sync.WaitGroup, plock *sync.Mutex, sem chan struct{}) {
	defer wg.Done()
	if fprx != nil && !fprx.MatchString(path) {
		return
	}
	if rx == nil {
		ms := fprx.FindAllStringIndex(path, -1)
		last := 0
		plock.Lock()
		defer plock.Unlock()
		for _, m := range ms {
			l, r := m[0], m[1]
			fmt.Printf("%s%s", path[last:l], colors.Bold(colors.Blue(path[l:r])))
			last = r
		}
		fmt.Printf("%s\n", path[last:])
		return
	}
	sem <- struct{}{}
	defer func(sem chan struct{}) {
		<-sem
	}(sem)

	var f *os.File
	var err error
	for {
		f, err = os.Open(path)
		if err != nil {
			if !tooManyOpen.MatchString(err.Error()) {
				log.Println(err)
				return
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}
		break
	}
	defer f.Close()
	grepReader(path, f, rx, plock)
}

func formatHeader(path string, num int) string {
	return fmt.Sprintf("%s (%d matches)\n", colors.Green(path), num)
}

func formatLine(line []byte, l, r, linenum, i int) string {
	s := fmt.Sprintf("%s%s%s",
		line[0:l],
		colors.Bold(colors.Blue(line[l:r])),
		line[r:],
	)
	if i == 0 {
		s = fmt.Sprintf("%s:\t", colors.BrightBlack(strconv.Itoa(linenum))) + s
	}
	return s
}
