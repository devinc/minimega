// Copyright (2012) Sandia Corporation.
// Under the terms of Contract DE-AC04-94AL85000 with Sandia Corporation,
// the U.S. Government retains certain rights in this software.
//Author: Brian Wright

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"minicli"
	log "minilog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const (
	defaultWebPort = 9001
	defaultWebRoot = "misc/web"
	friendlyError  = "oops, something went wrong"
)

type htmlTable struct {
	Header  []string
	Toggle  map[string]int
	Tabular [][]interface{}
	ID      string
	Class   string
}

type vmScreenshotParams struct {
	Host string
	Name string
	Port int
	ID   int
	Size int
}

var web struct {
	Running   bool
	Server    *http.Server
	Templates *template.Template
	Port      int
}

var webCLIHandlers = []minicli.Handler{
	{ // web
		HelpShort: "start the minimega webserver",
		HelpLong: `
Launch the minimega webserver. Running web starts the HTTP server whose port
cannot be changed once started. The default port is 9001. To run the server on
a different port, run:

	web 10000

The webserver requires several resources found in misc/web in the repo. By
default, it looks in $PWD/misc/web for these resources. If you are running
minimega from a different location, you can specify a different path using:

	web root <path/to/web/dir>

You can also set the port when starting web with an alternative root directory:

	web root <path/to/web/dir> 10000

NOTE: If you start the webserver with an invalid root, you can safely re-run
"web root" to update it. You cannot, however, change the server's port.`,
		Patterns: []string{
			"web [port]",
			"web root <path> [port]",
		},
		Call: wrapSimpleCLI(cliWeb),
	},
}

func init() {
	registerHandlers("web", webCLIHandlers)

}

func cliWeb(c *minicli.Command) *minicli.Response {
	resp := &minicli.Response{Host: hostname}

	port := defaultWebPort
	if c.StringArgs["port"] != "" {
		// Check if port is an integer
		p, err := strconv.Atoi(c.StringArgs["port"])
		if err != nil {
			resp.Error = fmt.Sprintf("'%v' is not a valid port", c.StringArgs["port"])
			return resp
		}

		port = p
	}

	root := defaultWebRoot
	if c.StringArgs["path"] != "" {
		root = c.StringArgs["path"]
	}

	go webStart(port, root)

	return resp
}

func webStart(port int, root string) {
	// Initialize templates
	templates := filepath.Join(root, "templates")
	log.Info("compiling templates from %s", templates)

	web.Templates = template.New("minimega-templates")
	filepath.Walk(templates, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			log.Error("failed to load template from %s", path)
			return nil
		}

		if !info.IsDir() && strings.HasSuffix(path, ".html") {
			web.Templates.ParseFiles(path)
		}

		return nil
	})

	mux := http.NewServeMux()
	for _, v := range []string{"novnc", "libs", "include"} {
		path := fmt.Sprintf("/%s/", v)
		dir := http.Dir(filepath.Join(root, v))
		mux.Handle(path, http.StripPrefix(path, http.FileServer(dir)))
	}

	mux.HandleFunc("/", webVMs)
	mux.HandleFunc("/map", webMapVMs)
	mux.HandleFunc("/screenshot/", webScreenshot)
	mux.HandleFunc("/hosts", webHosts)
	mux.HandleFunc("/tags", webVMTags)
	mux.HandleFunc("/tiles", webTileVMs)
	mux.HandleFunc("/graph", webGraph)
	mux.HandleFunc("/json", webJSON)
	mux.HandleFunc("/vnc/", webVNC)
	mux.HandleFunc("/ws/", vncWsHandler)

	if web.Server == nil {
		web.Server = &http.Server{
			Addr:    fmt.Sprintf(":%d", port),
			Handler: mux,
		}

		err := web.Server.ListenAndServe()
		if err != nil {
			log.Error("web: %v", err)
			web.Server = nil
		} else {
			web.Port = port
			web.Running = true
		}
	} else {
		log.Info("web: changing web root to: %s", root)
		if port != web.Port && port != defaultWebPort {
			log.Error("web: changing web's port is not supported")
		}
		// just update the mux
		web.Server.Handler = mux
	}
}

// webRenderTemplate renders the given template with the provided data, writing
// the result to the client. Should be called last in an http handler.
func webRenderTemplate(w http.ResponseWriter, tmpl string, data interface{}) {
	if err := web.Templates.ExecuteTemplate(w, tmpl, data); err != nil {
		log.Error("unable to execute template %s -- %v", tmpl, err)
		http.Error(w, friendlyError, http.StatusInternalServerError)
	}
}

// webScreenshot serves routes like /screenshot/<host>/<id>.png. Optional size
// query parameter dictates the size of the screenshot.
func webScreenshot(w http.ResponseWriter, r *http.Request) {
	fields := strings.Split(r.URL.Path, "/")
	if len(fields) != 4 {
		http.NotFound(w, r)
		return
	}
	fields = fields[2:]

	size := r.URL.Query().Get("size")
	host := fields[0]
	id := strings.TrimSuffix(fields[1], ".png")

	cmdStr := fmt.Sprintf("vm screenshot %s %s", id, size)
	if host != hostname {
		cmdStr = fmt.Sprintf("mesh send %s .record false %s", host, cmdStr)
	}

	cmd := minicli.MustCompile(cmdStr)
	cmd.Record = false

	var screenshot []byte

	for resps := range runCommand(cmd) {
		for _, resp := range resps {
			if resp.Error != "" {
				log.Errorln(resp.Error)
				http.Error(w, friendlyError, http.StatusInternalServerError)
				continue
			}

			if resp.Data == nil {
				http.NotFound(w, r)
			}

			if screenshot == nil {
				screenshot = resp.Data.([]byte)
			} else {
				log.Error("received more than one response for vm screenshot")
			}
		}
	}

	if screenshot != nil {
		w.Write(screenshot)
	} else {
		http.NotFound(w, r)
	}
}

func webGraph(w http.ResponseWriter, r *http.Request) {
	webRenderTemplate(w, "graph.html", make([]interface{}, 0))
}

// webVNC serves routes like /vnc/<host>/<port>/<vmName>.
func webVNC(w http.ResponseWriter, r *http.Request) {
	fields := strings.Split(r.URL.Path, "/")
	if len(fields) != 5 {
		http.NotFound(w, r)
		return
	}
	fields = fields[2:]

	host := fields[0]
	port := fields[1]
	vm := fields[2]

	data := struct {
		Title, Path string
	}{
		Title: fmt.Sprintf("%s:%s", host, vm),
		Path:  fmt.Sprintf("ws/%s/%s", host, port),
	}

	webRenderTemplate(w, "vnc.html", data)
}

func webMapVMs(w http.ResponseWriter, r *http.Request) {
	var err error

	type point struct {
		Lat, Long float64
		Text      string
	}

	points := []point{}

	for _, vms := range globalVmInfo() {
		for _, vm := range vms {
			name := fmt.Sprintf("%v:%v", vm.GetID(), vm.GetName())

			p := point{Text: name}

			if vm.Tag("lat") == "" || vm.Tag("long") == "" {
				log.Debug("skipping vm %s -- missing required tags lat/long", name)
				continue
			}

			p.Lat, err = strconv.ParseFloat(vm.Tag("lat"), 64)
			if err != nil {
				log.Error("invalid lat for vm %s -- expected float")
				continue
			}

			p.Long, err = strconv.ParseFloat(vm.Tag("lat"), 64)
			if err != nil {
				log.Error("invalid lat for vm %s -- expected float")
				continue
			}

			points = append(points, p)
		}
	}

	webRenderTemplate(w, "map.html", points)
}

func webVMTags(w http.ResponseWriter, r *http.Request) {
	table := htmlTable{
		Header:  []string{},
		Toggle:  map[string]int{},
		Tabular: [][]interface{}{},
	}

	tags := map[string]bool{}

	info := globalVmInfo()

	// Find all the distinct tags across all VMs
	for _, vms := range info {
		for _, vm := range vms {
			for _, k := range vm.GetTags() {
				tags[k] = true
			}
		}
	}

	fixedCols := []string{"Host", "Name", "ID"}

	// Copy into Header
	for k := range tags {
		table.Header = append(table.Header, k)
	}
	sort.Strings(table.Header)

	// Set up Toggle, offset by fixedCols which will be on the left
	for i, v := range table.Header {
		table.Toggle[v] = i + len(fixedCols)
	}

	// Update the VM's tags so that it contains all the distinct values and
	// then populate data
	for host, vms := range info {
		for _, vm := range vms {
			row := []interface{}{
				host,
				vm.GetName(),
				vm.GetID(),
			}

			for _, k := range table.Header {
				// If key is not present, will set it to the zero-value
				row = append(row, vm.Tag(k))
			}

			table.Tabular = append(table.Tabular, row)
		}
	}

	// Add "fixed" headers for host/...
	table.Header = append(fixedCols, table.Header...)

	webRenderTemplate(w, "tags.html", table)
}

func webHosts(w http.ResponseWriter, r *http.Request) {
	table := htmlTable{
		Header:  []string{},
		Tabular: [][]interface{}{},
		ID:      "example",
		Class:   "hover",
	}

	cmd := minicli.MustCompile("host")
	cmd.Record = false

	for resps := range runCommandGlobally(cmd) {
		for _, resp := range resps {
			if resp.Error != "" {
				log.Errorln(resp.Error)
				continue
			}

			if len(table.Header) == 0 && len(resp.Header) > 0 {
				table.Header = append(table.Header, resp.Header...)
			}

			for _, row := range resp.Tabular {
				res := []interface{}{}
				for _, v := range row {
					res = append(res, v)
				}
				table.Tabular = append(table.Tabular, res)
			}
		}
	}

	webRenderTemplate(w, "hosts.html", table)
}

func webVMs(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	table := htmlTable{
		Header:  []string{"host", "screenshot"},
		Tabular: [][]interface{}{},
		ID:      "example",
		Class:   "hover",
	}
	table.Header = append(table.Header, vmMasks...)

	stateMask := VM_QUIT | VM_ERROR

	for host, vms := range globalVmInfo() {
	vmLoop:
		for _, vm := range vms {
			var buf bytes.Buffer
			if vm.GetState()&stateMask == 0 {
				params := vmScreenshotParams{
					Host: host,
					Name: vm.GetName(),
					Port: 5900 + vm.GetID(),
					ID:   vm.GetID(),
					Size: 140,
				}

				if err := web.Templates.ExecuteTemplate(&buf, "fragment/screenshot", &params); err != nil {
					log.Error("unable to execute template screenshot -- %v", err)
					continue
				}
			}

			res := []interface{}{host, template.HTML(buf.String())}

			for _, mask := range vmMasks {
				if v, err := vm.Info(mask); err != nil {
					log.Error("bad mask for %v -- %v", vm.GetID(), err)
					continue vmLoop
				} else {
					res = append(res, v)
				}
			}

			table.Tabular = append(table.Tabular, res)
		}
	}

	webRenderTemplate(w, "table.html", table)
}

func webTileVMs(w http.ResponseWriter, r *http.Request) {
	stateMask := VM_QUIT | VM_ERROR

	params := []vmScreenshotParams{}

	for host, vms := range globalVmInfo() {
		for _, vm := range vms {
			if vm.GetState()&stateMask != 0 {
				continue
			}

			params = append(params, vmScreenshotParams{
				Host: host,
				Name: vm.GetName(),
				Port: 5900 + vm.GetID(),
				ID:   vm.GetID(),
				Size: 250,
			})
		}
	}

	webRenderTemplate(w, "tiles.html", params)
}

func webJSON(w http.ResponseWriter, r *http.Request) {
	// we want a map of "hostname + id" to vm info so that it can be sorted
	infovms := make(map[string]map[string]interface{}, 0)

	for host, vms := range globalVmInfo() {
		for _, vm := range vms {
			stateMask := VM_QUIT | VM_ERROR

			if vm.GetState()&stateMask != 0 {
				continue
			}

			config := vm.Config()

			vmMap := map[string]interface{}{
				"host": host,

				"id":    vm.GetID(),
				"name":  vm.GetName(),
				"state": vm.GetState().String(),
				"type":  vm.GetType().String(),

				"vcpus":  config.Vcpus,
				"memory": config.Memory,
			}

			if config.Networks == nil {
				vmMap["network"] = make([]int, 0)
			} else {
				vmMap["network"] = config.Networks
			}

			if vm.GetTags() == nil {
				vmMap["tags"] = make(map[string]string, 0)
			} else {
				vmMap["tags"] = vm.GetTags()
			}

			// The " " is invalid as a hostname, so we use it as a separator.
			infovms[host+" "+strconv.Itoa(vm.GetID())] = vmMap
		}
	}

	// We need to pass it as an array for the JSON generation (so the weird keys don't show up)
	infoslice := make([]map[string]interface{}, len(infovms))

	// Make a slice of all keys in infovms, then sort it
	keys := []string{}
	for k, _ := range infovms {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	// Make a sorted slice of values from the sorted slice of keys
	for i, k := range keys {
		infoslice[i] = infovms[k]
	}

	// Now the order of items in the JSON doesn't randomly change between calls (since the values are sorted)
	js, err := json.Marshal(infoslice)

	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(js)
}
