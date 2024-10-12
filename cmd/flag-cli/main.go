package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: flagctl projects|flags|audit list|create|enable|disable|rollout|archive [options]")
	}
	resource, command := args[0], args[1]
	fs := flag.NewFlagSet("flagctl", flag.ContinueOnError)
	endpoint := fs.String("endpoint", "http://localhost:18080", "server URL")
	token := fs.String("token", os.Getenv("FLAGFORGE_ADMIN_TOKEN"), "API token (prefer ENV)")
	project := fs.String("project", "", "project key")
	env := fs.String("env", "production", "environment key")
	key := fs.String("flag", "", "flag key")
	name := fs.String("name", "", "display name")
	kind := fs.String("type", "BOOLEAN", "flag type")
	percentage := fs.Int("percentage", 10, "rollout percentage")
	variation := fs.String("variation", "on", "rollout variation")
	file := fs.String("file", "", "JSON request file")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	if *token == "" {
		return fmt.Errorf("admin token required")
	}
	httpClient := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	request := func(method, path string, body []byte, version string) ([]byte, error) {
		req, err := http.NewRequest(method, *endpoint+path, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+*token)
		req.Header.Set("Content-Type", "application/json")
		if version != "" {
			req.Header.Set("If-Match", version)
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("HTTP request failed")
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		if err != nil {
			return nil, err
		}
		if resp.StatusCode >= 300 {
			return nil, fmt.Errorf("status %d: %s", resp.StatusCode, b)
		}
		return b, nil
	}
	method, path, version := http.MethodGet, "", ""
	var body []byte
	if resource == "projects" && command == "list" {
		path = "/api/v1/projects"
	} else if resource == "audit" && command == "list" {
		path = "/api/v1/audit?project=" + url.QueryEscape(*project)
	} else if resource == "flags" {
		if *project == "" {
			return fmt.Errorf("--project required")
		}
		base := "/api/v1/projects/" + url.PathEscape(*project) + "/flags"
		path = base
		switch command {
		case "list":
		case "create":
			method = http.MethodPost
			body, _ = json.Marshal(map[string]string{"key": *key, "name": *name, "type": *kind})
		case "archive":
			if *key == "" {
				return fmt.Errorf("--flag required")
			}
			method = http.MethodDelete
			path = base + "/" + url.PathEscape(*key)
		case "enable", "disable", "rollout":
			if *key == "" {
				return fmt.Errorf("--flag required")
			}
			base = "/api/v1/projects/" + url.PathEscape(*project) + "/environments/" + url.PathEscape(*env) + "/flags/" + url.PathEscape(*key)
			b, err := request(http.MethodGet, base, nil, "")
			if err != nil {
				return err
			}
			var f map[string]any
			if err = json.Unmarshal(b, &f); err != nil {
				return err
			}
			version = strconv.FormatInt(int64(f["version"].(float64)), 10)
			path = base + "/" + command
			method = http.MethodPost
			if command == "rollout" {
				if *percentage < 0 || *percentage > 100 {
					return fmt.Errorf("percentage must be 0..100")
				}
				off := f["off_variation"].(string)
				if off == *variation {
					return fmt.Errorf("rollout variation must differ from off variation")
				}
				f["enabled"] = true
				f["rollout"] = []map[string]any{{"variation_key": *variation, "weight": *percentage * 1000}, {"variation_key": off, "weight": 100000 - *percentage*1000}}
				body, _ = json.Marshal(f)
				method = http.MethodPut
				path = base
			}
		default:
			return fmt.Errorf("unsupported command")
		}
	} else {
		return fmt.Errorf("unsupported command")
	}
	if *file != "" {
		var err error
		body, err = os.ReadFile(*file)
		if err != nil {
			return err
		}
	}
	b, err := request(method, path, body, version)
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}
