package server

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"strings"
	"time"

	"github.com/ije/gox/utils"
)

type GitRef struct {
	Ref string
	Sha string
}

// list repo refs using `git ls-remote repo`
func listRepoRefs(repo string) (refs []GitRef, err error) {
	return withCache(fmt.Sprintf("git ls-remote %s", repo), time.Duration(config.NpmQueryCacheTTL)*time.Second, func() ([]GitRef, error) {
		cmd := exec.Command("git", "ls-remote", repo)
		stdout := bytes.NewBuffer(nil)
		errout := bytes.NewBuffer(nil)
		cmd.Stdout = stdout
		cmd.Stderr = errout
		err = cmd.Run()
		if err != nil {
			if errout.Len() > 0 {
				return nil, errors.New(errout.String())
			}
			return nil, err
		}
		refs = make([]GitRef, 0)
		r := bufio.NewReader(stdout)
		for {
			var line []byte
			line, err = r.ReadBytes('\n')
			if err == io.EOF {
				err = nil
				break
			}
			if err != nil {
				return nil, err
			}
			sha, ref := utils.SplitByLastByte(string(bytes.TrimSpace(line)), '\t')
			refs = append(refs, GitRef{
				Ref: ref,
				Sha: sha,
			})
		}
		return refs, nil
	})
}

func ghInstall(wd, name, hash string) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	url := fmt.Sprintf("https://codeload.github.com/%s/tar.gz/%s", name, hash)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	defer res.Body.Close()

	if res.StatusCode != 200 {
		return fmt.Errorf("fetch %s failed: %s", url, res.Status)
	}

	// ungzip tarball
	unziped, err := gzip.NewReader(res.Body)
	if err != nil {
		return
	}

	// extract tarball
	tr := tar.NewReader(unziped)
	rootDir := path.Join(wd, "node_modules", name)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		// strip tarball root dir
		name := strings.Join(strings.Split(h.Name, "/")[1:], "/")
		if strings.HasPrefix(name, ".") {
			continue
		}
		fp := path.Join(rootDir, name)
		if h.Typeflag == tar.TypeDir {
			ensureDir(fp)
			continue
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		extname := path.Ext(fp)
		if !(extname != "" && (assetExts[extname[1:]] || contains(moduleExts, extname) || extname == ".map" || extname == ".css" || extname == ".svelte" || extname == ".vue")) {
			// skip unsupported formats
			continue
		}
		f, err := os.OpenFile(fp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0644)
		if err != nil {
			return err
		}
		_, err = io.Copy(f, tr)
		f.Close()
		if err != nil {
			return err
		}
	}
	return
}
