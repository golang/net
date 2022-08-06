// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build ignore
// +build ignore

//go:generate go run gen.go

// This program generates system adaptation constants and types,
// internet protocol constants and tables by reading template files
// and IANA protocol registries.
package main

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"go/format"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

func main() {
	if err := genzsys(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := geniana(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func genzsys() error {
	defs := "defs_" + runtime.GOOS + ".go"
	f, err := os.Open(defs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	f.Close()
	cmd := exec.Command("go", "tool", "cgo", "-godefs", defs)
	b, err := cmd.Output()
	if err != nil {
		return err
	}
	b, err = format.Source(b)
	if err != nil {
		return err
	}
	zsys := "zsys_" + runtime.GOOS + ".go"
	switch runtime.GOOS {
	case "freebsd", "linux":
		zsys = "zsys_" + runtime.GOOS + "_" + runtime.GOARCH + ".go"
	}
	if err := os.WriteFile(zsys, b, 0644); err != nil {
		return err
	}
	return nil
}

var registries = []struct {
	url   string
	parse func(io.Writer, io.Reader) error
}{
	{
		"https://www.iana.org/assignments/icmpv6-parameters/icmpv6-parameters.xml",
		parseICMPv6Parameters,
	},
}

func geniana() error {
	var bb bytes.Buffer
	fmt.Fprintf(&bb, "// go generate gen.go\n")
	fmt.Fprintf(&bb, "// Code generated by the command above; DO NOT EDIT.\n\n")
	fmt.Fprintf(&bb, "package ipv6\n\n")
	for _, r := range registries {
		resp, err := http.Get(r.url)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("got HTTP status code %v for %v\n", resp.StatusCode, r.url)
		}
		if err := r.parse(&bb, resp.Body); err != nil {
			return err
		}
		fmt.Fprintf(&bb, "\n")
	}
	b, err := format.Source(bb.Bytes())
	if err != nil {
		return err
	}
	if err := os.WriteFile("iana.go", b, 0644); err != nil {
		return err
	}
	return nil
}

func parseICMPv6Parameters(w io.Writer, r io.Reader) error {
	dec := xml.NewDecoder(r)
	var icp icmpv6Parameters
	if err := dec.Decode(&icp); err != nil {
		return err
	}
	prs := icp.escape()
	fmt.Fprintf(w, "// %s, Updated: %s\n", icp.Title, icp.Updated)
	fmt.Fprintf(w, "const (\n")
	for _, pr := range prs {
		if pr.Name == "" {
			continue
		}
		fmt.Fprintf(w, "ICMPType%s ICMPType = %d", pr.Name, pr.Value)
		fmt.Fprintf(w, "// %s\n", pr.OrigName)
	}
	fmt.Fprintf(w, ")\n\n")
	fmt.Fprintf(w, "// %s, Updated: %s\n", icp.Title, icp.Updated)
	fmt.Fprintf(w, "var icmpTypes = map[ICMPType]string{\n")
	for _, pr := range prs {
		if pr.Name == "" {
			continue
		}
		fmt.Fprintf(w, "%d: %q,\n", pr.Value, strings.ToLower(pr.OrigName))
	}
	fmt.Fprintf(w, "}\n")
	return nil
}

type icmpv6Parameters struct {
	XMLName    xml.Name `xml:"registry"`
	Title      string   `xml:"title"`
	Updated    string   `xml:"updated"`
	Registries []struct {
		Title   string `xml:"title"`
		Records []struct {
			Value string `xml:"value"`
			Name  string `xml:"name"`
		} `xml:"record"`
	} `xml:"registry"`
}

type canonICMPv6ParamRecord struct {
	OrigName string
	Name     string
	Value    int
}

func (icp *icmpv6Parameters) escape() []canonICMPv6ParamRecord {
	id := -1
	for i, r := range icp.Registries {
		if strings.Contains(r.Title, "Type") || strings.Contains(r.Title, "type") {
			id = i
			break
		}
	}
	if id < 0 {
		return nil
	}
	prs := make([]canonICMPv6ParamRecord, len(icp.Registries[id].Records))
	sr := strings.NewReplacer(
		"Messages", "",
		"Message", "",
		"ICMP", "",
		"+", "P",
		"-", "",
		"/", "",
		".", "",
		" ", "",
	)
	for i, pr := range icp.Registries[id].Records {
		if strings.Contains(pr.Name, "Reserved") ||
			strings.Contains(pr.Name, "Unassigned") ||
			strings.Contains(pr.Name, "Deprecated") ||
			strings.Contains(pr.Name, "Experiment") ||
			strings.Contains(pr.Name, "experiment") {
			continue
		}
		ss := strings.Split(pr.Name, "\n")
		if len(ss) > 1 {
			prs[i].Name = strings.Join(ss, " ")
		} else {
			prs[i].Name = ss[0]
		}
		s := strings.TrimSpace(prs[i].Name)
		prs[i].OrigName = s
		prs[i].Name = sr.Replace(s)
		prs[i].Value, _ = strconv.Atoi(pr.Value)
	}
	return prs
}
