package main

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"unicode"

	alpm "github.com/jguer/go-alpm"
	rpc "github.com/mikkeloscar/aur"
	pkgb "github.com/mikkeloscar/gopkgbuild"
)

// upgrade type describes a system upgrade.
type upgrade struct {
	Name          string
	Repository    string
	LocalVersion  string
	RemoteVersion string
}

// upSlice is a slice of Upgrades
type upSlice []upgrade

func (u upSlice) Len() int      { return len(u) }
func (u upSlice) Swap(i, j int) { u[i], u[j] = u[j], u[i] }

func (u upSlice) Less(i, j int) bool {
	iRunes := []rune(u[i].Repository)
	jRunes := []rune(u[j].Repository)

	max := len(iRunes)
	if max > len(jRunes) {
		max = len(jRunes)
	}

	for idx := 0; idx < max; idx++ {
		ir := iRunes[idx]
		jr := jRunes[idx]

		lir := unicode.ToLower(ir)
		ljr := unicode.ToLower(jr)

		if lir != ljr {
			return lir > ljr
		}

		// the lowercase runes are the same, so compare the original
		if ir != jr {
			return ir > jr
		}
	}

	return false
}

// Print prints the details of the packages to upgrade.
func (u upSlice) Print(start int) {
	for k, i := range u {
		old, errOld := pkgb.NewCompleteVersion(i.LocalVersion)
		new, errNew := pkgb.NewCompleteVersion(i.RemoteVersion)
		var left, right string

		f := func(name string) (output string) {
			var hash = 5381
			for i := 0; i < len(name); i++ {
				hash = int(name[i]) + ((hash << 5) + (hash))
			}
			return fmt.Sprintf("\x1b[1;%dm%s\x1b[0m", hash%6+31, name)
		}
		fmt.Print(yellowFg(fmt.Sprintf("%2d ", len(u)+start-k-1)))
		fmt.Print(f(i.Repository), "/", boldWhiteFg(i.Name))

		if errOld != nil {
			left = redFg("Invalid Version")
		} else {
			if old.Version == new.Version {
				left = string(old.Version) + "-" + redFg(string(old.Pkgrel))
			} else {
				left = redFg(string(old.Version)) + "-" + string(old.Pkgrel)
			}
		}

		if errNew != nil {
			right = redFg("Invalid Version")
		} else {
			if old.Version == new.Version {
				right = string(new.Version) + "-" + greenFg(string(new.Pkgrel))
			} else {
				right = boldGreenFg(string(new.Version)) + "-" + string(new.Pkgrel)
			}
		}

		w := 70 - len(i.Repository) - len(i.Name) + len(left)
		fmt.Printf(fmt.Sprintf("%%%ds", w),
			fmt.Sprintf("%s -> %s\n", left, right))
	}
}

// upList returns lists of packages to upgrade from each source.
func upList() (aurUp upSlice, repoUp upSlice, err error) {
	local, remote, _, remoteNames, err := filterPackages()
	if err != nil {
		return
	}

	repoC := make(chan upSlice)
	aurC := make(chan upSlice)
	errC := make(chan error)

	fmt.Println(boldCyanFg("::"), boldFg("Searching databases for updates..."))
	go func() {
		repoUpList, err := upRepo(local)
		errC <- err
		repoC <- repoUpList
	}()

	fmt.Println(boldCyanFg("::"), boldFg("Searching AUR for updates..."))
	go func() {
		aurUpList, err := upAUR(remote, remoteNames)
		errC <- err
		aurC <- aurUpList
	}()

	var i = 0
loop:
	for {
		select {
		case repoUp = <-repoC:
			i++
		case aurUp = <-aurC:
			i++
		case err := <-errC:
			if err != nil {
				fmt.Println(err)
			}
		default:
			if i == 2 {
				close(repoC)
				close(aurC)
				close(errC)
				break loop
			}
		}
	}
	return
}

func upDevel(remote []alpm.Package, packageC chan upgrade, done chan bool) {
	for _, e := range savedInfo {
		if e.needsUpdate() {
			found := false
			var pkg alpm.Package
			for _, r := range remote {
				if r.Name() == e.Package {
					found = true
					pkg = r
				}
			}
			if found {
				if pkg.ShouldIgnore() {
					fmt.Print(yellowFg("Warning: "))
					fmt.Printf("%s ignoring package upgrade (%s => %s)\n", pkg.Name(), pkg.Version(), "git")
				} else {
					packageC <- upgrade{e.Package, "devel", e.SHA[0:6], "git"}
				}
			} else {
				removeVCSPackage([]string{e.Package})
			}
		}
	}
	done <- true
}

// upAUR gathers foreign packages and checks if they have new versions.
// Output: Upgrade type package list.
func upAUR(remote []alpm.Package, remoteNames []string) (toUpgrade upSlice, err error) {
	var j int
	var routines int
	var routineDone int

	packageC := make(chan upgrade)
	done := make(chan bool)

	if config.Devel {
		routines++
		go upDevel(remote, packageC, done)
		fmt.Println(boldCyanFg("::"), boldFg("Checking development packages..."))
	}

	for i := len(remote); i != 0; i = j {
		//Split requests so AUR RPC doesn't get mad at us.
		j = i - config.RequestSplitN
		if j < 0 {
			j = 0
		}

		routines++
		go func(local []alpm.Package, remote []string) {
			qtemp, err := rpc.Info(remote)
			if err != nil {
				fmt.Println(err)
				done <- true
				return
			}
			// For each item in query: Search equivalent in foreign.
			// We assume they're ordered and are returned ordered
			// and will only be missing if they don't exist in AUR.
			max := len(qtemp) - 1
			var missing, x int

			for i := range local {
				x = i - missing
				if x > max {
					break
				} else if qtemp[x].Name == local[i].Name() {
					if (config.TimeUpdate && (int64(qtemp[x].LastModified) > local[i].BuildDate().Unix())) ||
						(alpm.VerCmp(local[i].Version(), qtemp[x].Version) < 0) {
						if local[i].ShouldIgnore() {
							fmt.Print(yellowFg("Warning: "))
							fmt.Printf("%s ignoring package upgrade (%s => %s)\n", local[i].Name(), local[i].Version(), qtemp[x].Version)
						} else {
							packageC <- upgrade{qtemp[x].Name, "aur", local[i].Version(), qtemp[x].Version}
						}
					}
					continue
				} else {
					missing++
				}
			}
			done <- true
		}(remote[j:i], remoteNames[j:i])
	}

	for {
		select {
		case pkg := <-packageC:
			for _, w := range toUpgrade {
				if w.Name == pkg.Name {
					continue
				}
			}
			toUpgrade = append(toUpgrade, pkg)
		case <-done:
			routineDone++
			if routineDone == routines {
				err = nil
				return
			}
		}
	}
}

// upRepo gathers local packages and checks if they have new versions.
// Output: Upgrade type package list.
func upRepo(local []alpm.Package) (upSlice, error) {
	dbList, err := alpmHandle.SyncDbs()
	if err != nil {
		return nil, err
	}

	slice := upSlice{}

	for _, pkg := range local {
		newPkg := pkg.NewVersion(dbList)
		if newPkg != nil {
			if pkg.ShouldIgnore() {
				fmt.Print(yellowFg("Warning: "))
				fmt.Printf("%s ignoring package upgrade (%s => %s)\n", pkg.Name(), pkg.Version(), newPkg.Version())
			} else {
				slice = append(slice, upgrade{pkg.Name(), newPkg.DB().Name(), pkg.Version(), newPkg.Version()})
			}
		}
	}
	return slice, nil
}

//Contains returns whether e is present in s
func containsInt(s []int, e int) bool {
	for _, a := range s {
		if a == e {
			return true
		}
	}
	return false
}

// RemoveIntListFromList removes all src's elements that are present in target
func removeIntListFromList(src, target []int) []int {
	max := len(target)
	for i := 0; i < max; i++ {
		if containsInt(src, target[i]) {
			target = append(target[:i], target[i+1:]...)
			max--
			i--
		}
	}
	return target
}

// upgradePkgs handles updating the cache and installing updates.
func upgradePkgs(flags []string) error {
	aurUp, repoUp, err := upList()
	if err != nil {
		return err
	} else if len(aurUp)+len(repoUp) == 0 {
		fmt.Println("\nThere is nothing to do")
		return err
	}

	var repoNums []int
	var aurNums []int
	sort.Sort(repoUp)
	fmt.Println(boldBlueFg("::"), len(aurUp)+len(repoUp), boldWhiteFg("Packages to upgrade."))
	repoUp.Print(len(aurUp) + 1)
	aurUp.Print(1)

	if !config.NoConfirm {
		fmt.Println(greenFg("Enter packages you don't want to upgrade."))
		fmt.Print("Numbers: ")
		reader := bufio.NewReader(os.Stdin)

		numberBuf, overflow, err := reader.ReadLine()
		if err != nil || overflow {
			fmt.Println(err)
			return err
		}

		result := strings.Fields(string(numberBuf))
		excludeAur := make([]int, 0)
		excludeRepo := make([]int, 0)
		for _, numS := range result {
			negate := numS[0] == '^'
			if negate {
				numS = numS[1:]
			}
			var numbers []int
			num, err := strconv.Atoi(numS)
			if err != nil {
				numbers, err = BuildRange(numS)
				if err != nil {
					continue
				}
			} else {
				numbers = []int{num}
			}
			for _, target := range numbers {
				if target > len(aurUp)+len(repoUp) || target <= 0 {
					continue
				} else if target <= len(aurUp) {
					target = len(aurUp) - target
					if negate {
						excludeAur = append(excludeAur, target)
					} else {
						aurNums = append(aurNums, target)
					}
				} else {
					target = len(aurUp) + len(repoUp) - target
					if negate {
						excludeRepo = append(excludeRepo, target)
					} else {
						repoNums = append(repoNums, target)
					}
				}
			}
		}
		if len(repoNums) == 0 && len(aurNums) == 0 &&
			(len(excludeRepo) > 0 || len(excludeAur) > 0) {
			if len(repoUp) > 0 {
				repoNums = BuildIntRange(0, len(repoUp)-1)
			}
			if len(aurUp) > 0 {
				aurNums = BuildIntRange(0, len(aurUp)-1)
			}
		}
		aurNums = removeIntListFromList(excludeAur, aurNums)
		repoNums = removeIntListFromList(excludeRepo, repoNums)
	}

	arguments := cmdArgs.copy()
	arguments.delArg("u", "sysupgrade")
	arguments.delArg("y", "refresh")

	var repoNames []string
	var aurNames []string

	if len(repoUp) != 0 {
	repoloop:
		for i, k := range repoUp {
			for _, j := range repoNums {
				if j == i {
					continue repoloop
				}
			}
			repoNames = append(repoNames, k.Name)
		}
	}

	if len(aurUp) != 0 {
	aurloop:
		for i, k := range aurUp {
			for _, j := range aurNums {
				if j == i {
					continue aurloop
				}
			}
			aurNames = append(aurNames, k.Name)
		}
	}

	arguments.addTarget(repoNames...)
	arguments.addTarget(aurNames...)
	err = install(arguments)
	return err
}
