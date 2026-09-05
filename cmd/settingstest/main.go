// settingstest reads or patches a single field inside one of the camera's
// INF command-100 JSON documents (currently only DefaultInfo is JSON; see
// CAMERA-PROTOCOL.md section 7). Always writes back the full, freshly-read
// document with only the target field changed -- never a template, per the
// project's explicit warning against speculative partial writes.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"camera-service/internal/inf"
)

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func main() {
	host := flag.String("host", "10.1.0.21", "camera host")
	user := flag.String("user", envOr("CAMERA_USER", "admin"), "camera login user")
	password := flag.String("password", envOr("CAMERA_PASSWORD", ""), "camera login password (empty = mode-0 no-password login)")
	name := flag.String("name", "DefaultInfo", "settings function name")
	field := flag.String("field", "", "dotted path, e.g. image.mirror (empty = just print current document)")
	value := flag.String("value", "", "new integer value for -field; omit to only read")
	flag.Parse()

	creds := inf.Credentials{User: *user, Password: *password}
	dial := func() *inf.Conn {
		conn, err := inf.Dial(*host, 5*time.Second)
		if err != nil {
			log.Fatalf("dial: %v", err)
		}
		if err := conn.Login(creds); err != nil {
			log.Fatalf("login: %v", err)
		}
		return conn
	}

	getConn := dial()
	raw, err := getConn.SettingsGet(*name)
	getConn.Close()
	if err != nil {
		log.Fatalf("get %s: %v", *name, err)
	}
	if i := strings.IndexByte(string(raw), 0); i >= 0 {
		raw = raw[:i]
	}

	// Top-level only: every section we don't touch stays exactly the bytes
	// the camera sent us. Only the one target section gets reparsed and
	// rebuilt, matching the approach already verified to work in
	// internal/onvif/imaging.go (the AUTO round-trip test).
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		log.Fatalf("parse: %v (raw=%s)", err, raw)
	}

	if *field == "" {
		pretty, _ := json.MarshalIndent(doc, "", "  ")
		fmt.Println(string(pretty))
		return
	}
	parts := strings.SplitN(*field, ".", 2)
	if len(parts) != 2 {
		log.Fatalf("-field must be section.name, e.g. image.mirror")
	}
	section, key := parts[0], parts[1]

	secRaw, ok := doc[section]
	if !ok {
		log.Fatalf("no section %q in %s", section, *name)
	}
	var sec map[string]json.RawMessage
	if err := json.Unmarshal(secRaw, &sec); err != nil {
		log.Fatalf("parse section %q: %v", section, err)
	}
	var current int
	json.Unmarshal(sec[key], &current)
	fmt.Printf("%s.%s current value: %d\n", section, key, current)

	if *value == "" {
		return
	}
	nv, err := strconv.Atoi(*value)
	if err != nil {
		log.Fatalf("-value must be an integer")
	}
	patched, _ := json.Marshal(nv)
	sec[key] = patched
	secBody, err := json.Marshal(sec)
	if err != nil {
		log.Fatalf("marshal section: %v", err)
	}
	doc[section] = secBody

	body, err := json.Marshal(doc)
	if err != nil {
		log.Fatalf("marshal: %v", err)
	}
	setConn := dial()
	err = setConn.SettingsSet(*name, body)
	setConn.Close()
	if err != nil {
		log.Fatalf("set %s: %v", *name, err)
	}
	fmt.Printf("set %s.%s = %d (was %d), write acknowledged\n", section, key, nv, current)
}
