package main

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
	"strconv"

	"code.cloudfoundry.org/cli/plugin"
	"github.com/olekukonko/tablewriter"
)

// simpleClient is a simple CloudFoundry client
type simpleClient struct {
	// API url, ie "https://api.system.example.com"
	API string

	// Authorization header, ie "bearer eyXXXXX"
	Authorization string

	// Quiet - if set don't print progress to stderr
	Quiet bool

	// Client - http.Client to use
	Client *http.Client
}

// Get makes a GET request, where r is the relative path, and rv is json.Unmarshalled to
func (sc *simpleClient) Get(r string, rv interface{}) error {
	if !sc.Quiet {
		log.Printf("GET %s%s", sc.API, r)
	}
	req, err := http.NewRequest(http.MethodGet, sc.API+r, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", sc.Authorization)
	resp, err := sc.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return errors.New("bad status code")
	}

	return json.NewDecoder(resp.Body).Decode(rv)
}

// List makes a GET request, to list resources, where we will follow the "next_url"
// to page results, and calls "f" as a callback to process each resource found
func (sc *simpleClient) List(r string, f func(*resource) error) error {
	for r != "" {
		var res struct {
			NextURL   string `json:"next_url"`
			Resources []*resource
		}
		err := sc.Get(r, &res)
		if err != nil {
			return err
		}

		for _, rr := range res.Resources {
			err = f(rr)
			if err != nil {
				return err
			}
		}

		r = res.NextURL
	}
	return nil
}

// resource captures fields that we care about when
// retrieving data from CloudFoundry
type resource struct {
	Metadata struct {
		Guid      string    `json:"guid"`       // app
		UpdatedAt time.Time `json:"updated_at"` // buildpack
	} `json:"metadata"`
	Entity struct {
		Name               string    // org, space
		SpacesURL          string    `json:"spaces_url"`              // org
		UsersURL           string    `json:"users_url"`               // org
		ManagersURL        string    `json:"managers_url"`            // org, space
		BillingManagersURL string    `json:"billing_managers_url"`    // org
		AuditorsURL        string    `json:"auditors_url"`            // org, space
		DevelopersURL      string    `json:"developers_url"`          // space
		AppsURL            string    `json:"apps_url"`                // space
		DetectedBuildpack  string    `json:"detected_buildpack"`      // app
		Buildpack          string    `json:"buildpack"`               // app
		Memory			   int64       `json:"memory"`     	          // app
		Instances		   int64       `json:"instances"`     	       // app

		Admin              bool      // user
		Username           string    // user
		Filename           string    `json:"filename"`           // buildpack
		Enabled            bool      `json:"enabled"`            // buildpack
		PackageUpdatedAt   time.Time `json:"package_updated_at"` // app
	} `json:"entity"`
}

type droplet struct {
	Buildpacks []struct {
		Name          string `json:"name"`
		BuildpackName string `json:"buildpack_name"`
		Version       string `json:"version"`
	} `json:"buildpacks"`
}

type reportBuildpacks struct{}

func newSimpleClient(cliConnection plugin.CliConnection, quiet bool) (*simpleClient, error) {
	at, err := cliConnection.AccessToken()
	if err != nil {
		return nil, err
	}

	api, err := cliConnection.ApiEndpoint()
	if err != nil {
		return nil, err
	}

	skipSSL, err := cliConnection.IsSSLDisabled()
	if err != nil {
		return nil, err
	}

	httpClient := http.DefaultClient
	if skipSSL {
		if !quiet {
			log.Println("warning: skipping TLS validation...")
		}

		httpClient = &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					InsecureSkipVerify: true,
				},
			},
		}
	}

	return &simpleClient{
		API:           api,
		Authorization: at,
		Quiet:         quiet,
		Client:        httpClient,
	}, nil
}

func (c *reportBuildpacks) Run(cliConnection plugin.CliConnection, args []string) {
	outputJSON := false
	quiet := false

	fs := flag.NewFlagSet("report-buildpacks", flag.ExitOnError)
	fs.BoolVar(&outputJSON, "output-json", false, "if set sends JSON to stdout instead of a rendered table")
	fs.BoolVar(&quiet, "quiet", false, "if set suppressing printing of progress messages to stderr")
	err := fs.Parse(args[1:])
	if err != nil {
		log.Fatal(err)
	}

	client, err := newSimpleClient(cliConnection, quiet)
	if err != nil {
		log.Fatal(err)
	}

	switch args[0] {
	case "report-buildpacks":
		err := c.reportBuildpacks(client, os.Stdout, outputJSON)
		if err != nil {
			log.Fatal(err)
		}
	}
}

type buildpackUsageInfo struct {
	Organization string   `json:"organization"`
	Space        string   `json:"space"`
	Application  string   `json:"application"`
	Buildpacks   []string `json:"buildpacks,omitempty"`
	TotalMemory  string  `json:"total_memory,omitempty"`	
	Messages     []string `json:"messages,omitempty"`
}

func (c *reportBuildpacks) reportBuildpacks(client *simpleClient, out io.Writer, outputJSON bool) error {
	buildpacks := make(map[string]*resource)
	err := client.List("/v2/buildpacks", func(bp *resource) error {
		if bp.Entity.Enabled {
			buildpacks[bp.Entity.Name] = bp
		}
		return nil
	})
	if err != nil {
		return err
	}

	var allInfo []*buildpackUsageInfo
	err = client.List("/v2/organizations", func(org *resource) error {
		return client.List(org.Entity.SpacesURL, func(space *resource) error {
			return client.List(space.Entity.AppsURL, func(app *resource) error {
				var bps []string
				var messages []string

				var dropletAnswer droplet
				err := client.Get(fmt.Sprintf("/v3/apps/%s/droplets/current", app.Metadata.Guid), &dropletAnswer)
				if err != nil {
					messages = append(messages, "needs attention (1)")
				} else {
					if len(dropletAnswer.Buildpacks) == 0 {
						messages = append(messages, "needs attention (2)")
					}
					for _, bp := range dropletAnswer.Buildpacks {
						bps = append(bps, fmt.Sprintf("%s", bp.Name))
						if bp.Version == "" {
							bps = append(bps, fmt.Sprintf("%s", bp.BuildpackName))
							messages = append(messages, "needs attention (3)")
						} else {
							bps = append(bps, fmt.Sprintf("%s v%s", bp.BuildpackName, bp.Version))
							
							bpr, found := buildpacks[bp.Name]
							if !found {
								messages = append(messages, "needs attention (4)")
							} else {
								if !strings.HasSuffix(bpr.Entity.Filename, fmt.Sprintf("v%s.zip", bp.Version)) {
									messages = append(messages, "needs attention (5)")
								}
							}
						}
					}
				}

				if len(bps) == 0 {
					if app.Entity.Buildpack != "" {
						bps = append(bps, app.Entity.Buildpack)
					} else {
						if app.Entity.DetectedBuildpack != "" {
							bps = append(bps, app.Entity.DetectedBuildpack)
						}
					}
				}

				if len(messages) == 0 {
					messages = append(messages, "OK")
				}

				allInfo = append(allInfo, &buildpackUsageInfo{
					Organization: org.Entity.Name,
					Space:        space.Entity.Name,
					Application:  app.Entity.Name,
					Buildpacks:   bps,
					TotalMemory:   strconv.FormatInt (    app.Entity.Memory * app.Entity.Instances, 10 ),					
					Messages:     messages,
				})

				return nil
			})
		})
	})
	if err != nil {
		return err
	}

	if outputJSON {
		return json.NewEncoder(out).Encode(allInfo)
	}

	table := tablewriter.NewWriter(out)
	table.SetHeader([]string{"Organization", "Space", "Application", "Buildpacks", "Total Memory", "Messages"})
	for _, row := range allInfo {
		table.Append([]string{
			row.Organization,
			row.Space,
			row.Application,
			strings.Join(row.Buildpacks, ", "),
			row.TotalMemory,
			strings.Join(row.Messages, ", "),
		})
	}
	table.Render()

	return nil
}

func (c *reportBuildpacks) GetMetadata() plugin.PluginMetadata {
	return plugin.PluginMetadata{
		Name: "report-buildpacks",
		Version: plugin.VersionType{
			Major: 0,
			Minor: 2,
			Build: 0,
		},
		MinCliVersion: plugin.VersionType{
			Major: 6,
			Minor: 7,
			Build: 0,
		},
		Commands: []plugin.Command{
			{
				Name:     "report-buildpacks",
				HelpText: "Report all buildpacks used in installation",
				UsageDetails: plugin.Usage{
					Usage: "cf report-buildpacks",
					Options: map[string]string{
						"output-json": "if set sends JSON to stdout instead of a rendered table",
						"quiet":       "if set suppresses printing of progress messages to stderr",
					},
				},
			},
		},
	}
}

func main() {
	plugin.Start(&reportBuildpacks{})
}
