package kaas

import (
	"fmt"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/gorilla/websocket"
	"github.com/sirupsen/logrus"
)

const (
	charset    = "abcdefghijklmnopqrstuvwxyz"
	randLength = 8
)

var (
	// There's directories we know that definitely do not contain must-gathers, let's
	// save ourselves the trouble.
	ignoredPaths = []string{"namespaces", "cluster-scoped-resources", "gather-extra", "cloud.google.com"}

	// Files containing cluster dumps we want to hand off to KAS
	clusterDumps = []string{"must-gather.tar", "hypershift-dump.tar"}
)

func generateAppLabel() string {
	seededRand := rand.New(rand.NewSource(time.Now().UnixNano()))

	b := make([]byte, randLength)
	for i := range b {
		b[i] = charset[seededRand.Intn(len(charset))]
	}
	return string(b)
}

func getTarPaths(conn *websocket.Conn, url string) (*ProwInfo, error) {
	sendWSMessage(conn, "status", fmt.Sprintf("Finding artifacts for %s", url))
	// Get the URL for artifacts directory
	artifactURL, err := findArtifactURL("https://prow.ci.openshift.org/view/gs/origin-ci-test/logs/periodic-ci-openshift-hypershift-release-4.14-periodics-e2e-aws-ovn/1669918745679630336")
	if err != nil {
		return nil, fmt.Errorf("couldn't get artifact url: %+v", err)

	}
	sendWSMessage(conn, "status", fmt.Sprintf("Found artifact url: %s", artifactURL))

	dumpURLs, err := findURLsRecursively(artifactURL, clusterDumps)
	if err != nil {
		return nil, fmt.Errorf("couldn't fetch urls:% +v")
	}

	for _, u := range dumpURLs {
		sendWSMessage(conn, "status", fmt.Sprintf("Found dump archive at %s", u))
	}

	mgp := ""
	if len(dumpURLs) > 0 {
		mgp = dumpURLs[0]
	}

	return &ProwInfo{
		MustGatherURL: mgp,
		DumpURL:       dumpURLs,
	}, nil
}

func newDocument(url string) (*goquery.Document, error) {
	res, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status code error: %d %s", res.StatusCode, res.Status)
	}

	return goquery.NewDocumentFromReader(res.Body)
}

// findArtifactURL finds the "artifacts" directory path
func findArtifactURL(bucketURL string) (string, error) {
	logrus.Infof("finding artifacts url %s", bucketURL)
	doc, err := newDocument(bucketURL)
	if err != nil {
		return "", err
	}

	// See if we're on gcsweb
	selector := doc.Find("a:contains('artifacts/')")
	if selector.Length() == 0 {
		// We're not, maybe we're on prow
		selector = doc.Find("a:contains('Artifacts')")
		if selector.Length() != 0 {
			gcsURL, exists := selector.Attr("href")
			if !exists {
				return "", fmt.Errorf("couldn't find Artifacts link")
			}
			gcsURL, err = joinWithBaseURL(bucketURL, gcsURL)
			if err != nil {
				return "", err
			}
			logrus.Infof("have prow url, fetching gcsweb link")
			return findArtifactURL(gcsURL)
		}
	}

	artifactURL, exists := selector.Attr("href")
	if !exists {
		return "", fmt.Errorf("no href found for 'artifacts' link")
	}

	return joinWithBaseURL(bucketURL, artifactURL)
}

// find matching paths
func findURLsRecursively(url string, paths []string) ([]string, error) {
	logrus.Infof("processing %s", url)
	doc, err := newDocument(url)
	if err != nil {
		return nil, err
	}
	var urls []string

	doc.Find("a").Each(func(_ int, s *goquery.Selection) {
		for _, path := range paths {
			if strings.TrimSpace(s.Text()) == path {
				pathURL, _ := s.Attr("href")
				pathURL, err = joinWithBaseURL(url, pathURL)
				if err != nil {
					logrus.WithError(err).Warningf("couldn't build url")
					continue
				}

				urls = append(urls, pathURL)
			}
		}

		if strings.HasSuffix(s.Text(), "/") {
			subURL, exists := s.Attr("href")
			if exists && !isIgnoredPath(subURL) {
				subURL, _ = joinWithBaseURL(url, subURL)
				results, err := findURLsRecursively(subURL, paths)
				if err != nil {
					logrus.WithError(err).Warningf("encountered error at %s", subURL)
				}

				urls = append(urls, results...)
			}
		}
	})

	return urls, nil
}

func isIgnoredPath(subURL string) bool {
	for _, path := range ignoredPaths {
		if strings.Contains(subURL, path) {
			return true
		}
	}

	return false
}

func joinWithBaseURL(baseURL, path string) (string, error) {
	parsedBaseURL, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("couldn't parse bucket URL")
	}

	parsedRelativeURL, err := url.Parse(path)
	if err != nil {
		return "", fmt.Errorf("couldn't parse artifact URL")
	}

	return parsedBaseURL.ResolveReference(parsedRelativeURL).String(), nil
}

// All hypershift dumps are called "hypershift-dump.tar," we want to differentiate them
// so let's append the last 2 paths in the URL to the name.  On hypershift, this gives us
// the test case, e.g. artifacts-TestUpgradeControlPlane_PreTeardownClusterDump-hypershift-dump.tar
func joinedFilename(s string) string {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}

	p := u.Path
	elements := strings.Split(p, "/")
	n := len(elements)

	if n > 2 {
		return elements[n-3] + "-" + elements[n-2] + "-" + elements[n-1]
	} else if n > 1 {
		return elements[n-2] + "-" + elements[n-1]
	} else if n > 0 {
		return elements[n-1]
	}

	return ""
}
