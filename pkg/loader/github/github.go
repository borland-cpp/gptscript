package github

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/gptscript-ai/gptscript/pkg/loader"
	"github.com/gptscript-ai/gptscript/pkg/system"
)

const (
	GithubPrefix      = "github.com/"
	githubRepoURL     = "https://github.com/%s/%s.git"
	githubDownloadURL = "https://raw.githubusercontent.com/%s/%s/%s/%s"
	githubCommitURL   = "https://api.github.com/repos/%s/%s/commits/%s"
)

func init() {
	loader.AddVSC(Load)
}

func getCommit(account, repo, ref string) (string, error) {
	url := fmt.Sprintf(githubCommitURL, account, repo, ref)
	resp, err := http.Get(url)
	if err != nil {
		return "", err
	} else if resp.StatusCode != http.StatusOK {
		c, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return "", fmt.Errorf("failed to GitHub commit of %s/%s at %s: %s %s",
			account, repo, ref, resp.Status, c)
	}
	defer resp.Body.Close()

	var commit struct {
		SHA string `json:"sha,omitempty"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&commit); err != nil {
		return "", fmt.Errorf("failed to decode GitHub commit of %s/%s at %s: %w", account, repo, url, err)
	}

	if commit.SHA == "" {
		return "", fmt.Errorf("failed to find commit in response of %s, got empty string", url)
	}

	return commit.SHA, nil
}

func Load(urlName string) (string, *loader.Repo, bool, error) {
	if !strings.HasPrefix(urlName, GithubPrefix) {
		return "", nil, false, nil
	}

	url, ref, _ := strings.Cut(urlName, "@")
	if ref == "" {
		ref = "HEAD"
	}

	parts := strings.Split(url, "/")
	// Must be at least 4 parts github.com/ACCOUNT/REPO/FILE
	if len(parts) < 4 {
		return "", nil, false, nil
	}

	account, repo := parts[1], parts[2]
	path := strings.Join(parts[3:], "/")

	if path == "" || path == "/" {
		path = "tool.gpt"
	} else if !strings.HasSuffix(path, system.Suffix) {
		path += "/tool.gpt"
	}

	ref, err := getCommit(account, repo, ref)
	if err != nil {
		return "", nil, false, err
	}

	downloadURL := fmt.Sprintf(githubDownloadURL, account, repo, ref, path)
	return downloadURL, &loader.Repo{
		VCS:      "github",
		Root:     fmt.Sprintf(githubRepoURL, account, repo),
		Path:     filepath.Dir(path),
		Name:     filepath.Base(path),
		Revision: ref,
	}, true, nil
}
