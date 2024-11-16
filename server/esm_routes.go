package server

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/esm-dev/esm.sh/server/common"
	"github.com/esm-dev/esm.sh/server/storage"
	esbuild "github.com/evanw/esbuild/pkg/api"
	"github.com/ije/gox/crypto/rs"
	"github.com/ije/gox/utils"
	"github.com/ije/gox/valid"
	"github.com/ije/rex"
	"golang.org/x/net/html"
)

const (
	ccMustRevalidate = "public, max-age=0, must-revalidate"
	cc1hour          = "public, max-age=3600"
	cc1day           = "public, max-age=86400"
	ccImmutable      = "public, max-age=31536000, immutable"
	ctJavaScript     = "application/javascript; charset=utf-8"
	ctTypeScript     = "application/typescript; charset=utf-8"
	ctJSON           = "application/json; charset=utf-8"
	ctCSS            = "text/css; charset=utf-8"
	ctHtml           = "text/html; charset=utf-8"
)

func esmRoutes(debug bool) rex.Handle {
	startTime := time.Now()
	globalETag := fmt.Sprintf(`W/"v%d"`, VERSION)

	return func(ctx *rex.Context) any {
		pathname := ctx.Pathname()

		// ban malicious requests
		if strings.HasPrefix(pathname, "/.") || strings.HasSuffix(pathname, ".php") {
			return rex.Status(404, "not found")
		}

		// handle POST API requests
		switch ctx.R.Method {
		case "POST":
			switch pathname {
			case "/transform":
				var options TransformOptions
				err := json.NewDecoder(io.LimitReader(ctx.R.Body, 2*MB)).Decode(&options)
				ctx.R.Body.Close()
				if err != nil {
					return rex.Err(400, "require valid json body")
				}
				if options.Code == "" {
					return rex.Err(400, "Code is required")
				}
				if len(options.Code) > MB {
					return rex.Err(429, "Code is too large")
				}
				if targets[options.Target] == 0 {
					options.Target = "esnext"
				}
				if options.Lang == "" && options.Filename != "" {
					_, options.Lang = utils.SplitByLastByte(options.Filename, '.')
				}

				h := sha1.New()
				h.Write([]byte(options.Lang))
				h.Write([]byte(options.Code))
				h.Write([]byte(options.Target))
				h.Write(options.ImportMap)
				h.Write([]byte(options.JsxImportSource))
				h.Write([]byte(options.SourceMap))
				h.Write([]byte(fmt.Sprintf("%v", options.Minify)))
				hash := hex.EncodeToString(h.Sum(nil))

				// if previous build exists, return it directly
				savePath := fmt.Sprintf("modules/%s.mjs", hash)
				if file, _, err := esmStorage.Get(savePath); err == nil {
					data, err := io.ReadAll(file)
					file.Close()
					if err != nil {
						return rex.Err(500, "failed to read code")
					}
					output := TransformOutput{
						Code: string(data),
					}
					file, _, err = esmStorage.Get(savePath + ".map")
					if err == nil {
						data, err = io.ReadAll(file)
						file.Close()
						if err == nil {
							output.Map = string(data)
						}
					}
					return output
				}

				importMap := common.ImportMap{Imports: map[string]string{}}
				if len(options.ImportMap) > 0 {
					err = json.Unmarshal(options.ImportMap, &importMap)
					if err != nil {
						return rex.Err(400, "Invalid ImportMap")
					}
				}

				output, err := transform(&ResolvedTransformOptions{
					TransformOptions: options,
					importMap:        importMap,
				})
				if err != nil {
					return rex.Err(400, err.Error())
				}
				if len(output.Map) > 0 {
					output.Code = fmt.Sprintf("%s//# sourceMappingURL=+%s", output.Code, path.Base(savePath)+".map")
					go esmStorage.Put(savePath+".map", strings.NewReader(output.Map))
				}
				go esmStorage.Put(savePath, strings.NewReader(output.Code))
				ctx.SetHeader("Cache-Control", ccMustRevalidate)
				return output

			case "/purge":
				zoneId := ctx.FormValue("zoneId")
				packageName := ctx.FormValue("package")
				version := ctx.FormValue("version")
				if packageName == "" {
					return rex.Err(400, "param `package` is required")
				}
				if version != "" && !regexpVersionStrict.MatchString(version) {
					return rex.Err(400, "invalid version")
				}
				prefix := ""
				if zoneId != "" {
					prefix = zoneId + "/"
				}
				deletedBuildFiles, err := esmStorage.DeleteAll(prefix + "builds/" + packageName + "@" + version)
				if err != nil {
					return rex.Err(500, err.Error())
				}
				deletedDTSFiles, err := esmStorage.DeleteAll(prefix + "types/" + packageName + "@" + version)
				if err != nil {
					return rex.Err(500, err.Error())
				}
				deleteKeys := make([]string, len(deletedBuildFiles)+len(deletedDTSFiles))
				copy(deleteKeys, deletedBuildFiles)
				copy(deleteKeys[len(deletedBuildFiles):], deletedDTSFiles)
				log.Infof("Purged %d files for %s@%s (ip: %s)", len(deleteKeys), packageName, version, ctx.RemoteIP())
				return map[string]any{"deleted": deleteKeys}

			default:
				return rex.Err(404, "not found")
			}
		case "GET", "HEAD":
			// continue
		default:
			return rex.Err(405, "Method Not Allowed")
		}

		// strip trailing slash
		if pathname != "/" && strings.HasSuffix(pathname, "/") {
			pathname = strings.TrimRight(pathname, "/")
		}

		cdnOrigin := ctx.R.Header.Get("X-Real-Origin")
		if cdnOrigin == "" {
			proto := "http"
			if ctx.R.TLS != nil {
				proto = "https"
			}
			cdnOrigin = fmt.Sprintf("%s://%s", proto, ctx.R.Host)
		}

		// static routes
		switch pathname {
		case "/":
			ifNoneMatch := ctx.R.Header.Get("If-None-Match")
			if ifNoneMatch != "" && ifNoneMatch == globalETag {
				return rex.Status(http.StatusNotModified, nil)
			}
			indexHTML, err := embedFS.ReadFile("server/embed/index.html")
			if err != nil {
				return err
			}
			readme, err := embedFS.ReadFile("README.md")
			if err != nil {
				return err
			}
			readme = bytes.ReplaceAll(readme, []byte("./server/embed/"), []byte("/embed/"))
			readme = bytes.ReplaceAll(readme, []byte("./HOSTING.md"), []byte("https://github.com/esm-dev/esm.sh/blob/main/HOSTING.md"))
			readme = bytes.ReplaceAll(readme, []byte("https://esm.sh"), []byte(cdnOrigin))
			readmeStrLit := utils.MustEncodeJSON(string(readme))
			html := bytes.ReplaceAll(indexHTML, []byte("$README"), readmeStrLit)
			html = bytes.ReplaceAll(html, []byte("{VERSION}"), []byte(fmt.Sprintf("%d", VERSION)))
			ctx.SetHeader("Cache-Control", ccMustRevalidate)
			ctx.SetHeader("Etag", globalETag)
			return rex.Content("index.html", startTime, bytes.NewReader(html))

		case "/status.json":
			q := make([]map[string]any, buildQueue.queue.Len())
			i := 0

			buildQueue.lock.RLock()
			for el := buildQueue.queue.Front(); el != nil; el = el.Next() {
				t, ok := el.Value.(*BuildTask)
				clientIps := make([]string, len(t.clients))
				for idx, c := range t.clients {
					clientIps[idx] = c.IP
				}
				if ok {
					m := map[string]any{
						"clients":   clientIps,
						"createdAt": t.createdAt.Format(http.TimeFormat),
						"inProcess": t.inProcess,
						"path":      t.Path(),
						"stage":     t.stage,
					}
					if !t.startedAt.IsZero() {
						m["startedAt"] = t.startedAt.Format(http.TimeFormat)
					}
					q[i] = m
					i++
				}
			}
			buildQueue.lock.RUnlock()

			disk := "ok"
			tmpFilepath := path.Join(os.TempDir(), rs.Hex.String(32))
			err := os.WriteFile(tmpFilepath, make([]byte, MB), 0644)
			if err != nil {
				if errors.Is(err, syscall.ENOSPC) {
					disk = "full"
				} else {
					disk = "error"
				}
			}
			os.Remove(tmpFilepath)

			ctx.SetHeader("Cache-Control", ccMustRevalidate)
			return map[string]any{
				"buildQueue": q[:i],
				"version":    VERSION,
				"uptime":     time.Since(startTime).String(),
				"disk":       disk,
			}

		case "/error.js":
			switch query := ctx.Query(); query.Get("type") {
			case "resolve":
				return errorJS(ctx, fmt.Sprintf(
					`Could not resolve "%s" (Imported by "%s")`,
					query.Get("name"),
					query.Get("importer"),
				))
			case "unsupported-node-builtin-module":
				return errorJS(ctx, fmt.Sprintf(
					`Unsupported Node builtin module "%s" (Imported by "%s")`,
					query.Get("name"),
					query.Get("importer"),
				))
			case "unsupported-node-native-module":
				return errorJS(ctx, fmt.Sprintf(
					`Unsupported node native module "%s" (Imported by "%s")`,
					query.Get("name"),
					query.Get("importer"),
				))
			case "unsupported-npm-package":
				return errorJS(ctx, fmt.Sprintf(
					`Unsupported NPM package "%s" (Imported by "%s")`,
					query.Get("name"),
					query.Get("importer"),
				))
			case "unsupported-file-dependency":
				return errorJS(ctx, fmt.Sprintf(
					`Unsupported file dependency "%s" (Imported by "%s")`,
					query.Get("name"),
					query.Get("importer"),
				))
			default:
				return rex.Status(500, "Unknown error")
			}

		case "/favicon.ico":
			favicon, err := embedFS.ReadFile("server/embed/assets/favicon.ico")
			if err != nil {
				return err
			}
			ctx.SetHeader("Cache-Control", ccImmutable)
			return rex.Content("favicon.ico", startTime, bytes.NewReader(favicon))
		}

		// serve embed assets
		if strings.HasPrefix(pathname, "/embed/") {
			modTime := startTime
			if fs, ok := embedFS.(*MockEmbedFS); ok {
				if fi, err := fs.Lstat("server" + pathname); err == nil {
					modTime = fi.ModTime()
				}
			}
			data, err := embedFS.ReadFile("server" + pathname)
			if err != nil {
				return rex.Status(404, "not found")
			}
			ctx.SetHeader("Cache-Control", cc1day)
			return rex.Content(pathname, modTime, bytes.NewReader(data))
		}

		// strip loc suffix
		if strings.ContainsRune(pathname, ':') {
			pathname = regexpLocPath.ReplaceAllString(pathname, "$1")
		}

		// serve internal scripts
		if pathname == "/run" || pathname == "/tsx" || pathname == "/uno" {
			ifNoneMatch := ctx.R.Header.Get("If-None-Match")
			if ifNoneMatch == globalETag && !debug {
				return rex.Status(http.StatusNotModified, nil)
			}

			// determine build target by `?target` query or `User-Agent` header
			target := strings.ToLower(ctx.Query().Get("target"))
			targetFromUA := targets[target] == 0
			if targetFromUA {
				target = getBuildTargetByUA(ctx.UserAgent())
			}

			js, err := buildEmbedTS(pathname[1:]+".ts", target, debug)
			if err != nil {
				return rex.Status(500, fmt.Sprintf("Transform error: %v", err))
			}

			ctx.SetHeader("Cache-Control", cc1day)
			ctx.SetHeader("Etag", globalETag)
			if targetFromUA {
				appendVaryHeader(ctx.W.Header(), "User-Agent")
			}
			ctx.SetHeader("Content-Type", ctJavaScript)
			return js
		}

		// serve modules are generated by the /transform API
		if strings.HasPrefix(pathname, "/+") {
			hash, ext := utils.SplitByFirstByte(pathname[2:], '.')
			if len(hash) != 40 {
				return rex.Status(404, "Not Found")
			}
			savePath := fmt.Sprintf("modules/%s.%s", hash, ext)
			f, fi, err := esmStorage.Get(savePath)
			if err != nil {
				return rex.Status(500, err.Error())
			}
			if strings.HasSuffix(pathname, ".map") {
				ctx.SetHeader("Content-Type", ctJSON)
			} else {
				ctx.SetHeader("Content-Type", ctJavaScript)
			}
			ctx.SetHeader("Cache-Control", ccImmutable)
			return rex.Content(savePath, fi.ModTime(), f) // auto closed
		}

		// serve node libs
		if strings.HasPrefix(pathname, "/node/") && strings.HasSuffix(pathname, ".js") {
			lib, ok := nodeLibs[pathname[1:]]
			if !ok {
				// empty module
				lib = "export default {}"
			}
			if strings.HasPrefix(pathname, "/node/chunk-") {
				ctx.SetHeader("Cache-Control", ccImmutable)
			} else {
				ifNoneMatch := ctx.R.Header.Get("If-None-Match")
				if ifNoneMatch == globalETag && !debug {
					return rex.Status(http.StatusNotModified, nil)
				}
				ctx.SetHeader("Cache-Control", cc1day)
				ctx.SetHeader("Etag", globalETag)
			}
			target := getBuildTargetByUA(ctx.UserAgent())
			code, err := minify(lib, esbuild.LoaderJS, targets[target])
			if err != nil {
				return rex.Status(500, fmt.Sprintf("Transform error: %v", err))
			}
			ctx.SetHeader("Content-Type", ctJavaScript)
			appendVaryHeader(ctx.W.Header(), "User-Agent")
			return rex.Content(pathname, startTime, bytes.NewReader(code))
		}

		var npmrc *NpmRC
		if rc := ctx.R.Header.Get("X-Npmrc"); rc != "" {
			rc, err := NewNpmRcFromJSON([]byte(rc))
			if err != nil {
				return rex.Status(400, "Invalid Npmrc Header")
			}
			npmrc = rc
		} else {
			npmrc = NewNpmRcFromConfig()
		}

		zoneId := ctx.R.Header.Get("X-Zone-Id")
		if zoneId != "" {
			if !valid.IsDomain(zoneId) {
				zoneId = ""
			} else {
				var scopeName string
				if pkgName := toPackageName(pathname[1:]); strings.HasPrefix(pkgName, "@") {
					scopeName = pkgName[:strings.Index(pkgName, "/")]
				}
				if scopeName != "" {
					reg, ok := npmrc.Registries[scopeName]
					if !ok || (reg.Registry == jsrRegistry && reg.Token == "" && (reg.User == "" || reg.Password == "")) {
						zoneId = ""
					}
				} else if npmrc.Registry == npmRegistry && npmrc.Token == "" && (npmrc.User == "" || npmrc.Password == "") {
					zoneId = ""
				}
			}
		}
		if zoneId != "" {
			npmrc.zoneId = zoneId
			cdnOrigin = fmt.Sprintf("https://%s", zoneId)
		}

		if pathname == "/uno.css" {
			query := ctx.Query()
			ctxUrlRaw, err := atobUrl(query.Get("ctx"))
			if err != nil {
				return rex.Status(400, "Invalid context url")
			}
			ctxUrl, err := url.Parse(ctxUrlRaw)
			if err != nil {
				return rex.Status(400, "Invalid context url")
			}
			if ctxUrl.Scheme != "http" && ctxUrl.Scheme != "https" {
				return rex.Status(400, "Invalid context url")
			}
			hostname := ctxUrl.Hostname()
			// disallow localhost or ip address for production
			if !debug {
				if isLocalhost(hostname) {
					ctx.SetHeader("Cache-Control", ccImmutable)
					ctx.SetHeader("Content-Type", ctCSS)
					return "body:after{position:fixed;top:0;left:0;z-index:9999;padding:18px 32px;width:100vw;content:'esm.sh/uno doesn't support local development, try serving your app with `esm.sh run`.';font-size:14px;background:rgba(255,232,232,.9);color:#f00;backdrop-filter:blur(8px)}"
				}
				if !regexpDomain.MatchString(hostname) || ctxUrl.Host == ctx.R.Host {
					return rex.Status(400, "Invalid context url")
				}
			}
			// determine build target by `?target` query or `User-Agent` header
			target := strings.ToLower(query.Get("target"))
			if targets[target] == 0 {
				target = "es2022"
			}
			h := sha1.New()
			h.Write([]byte(ctxUrlRaw))
			h.Write([]byte(query.Get("v")))
			h.Write([]byte(target))
			savePath := normalizeSavePath(zoneId, path.Join("modules", hex.EncodeToString(h.Sum(nil))+".css"))
			content, _, err := esmStorage.Get(savePath)
			if err != nil && err != storage.ErrNotFound {
				return rex.Status(500, err.Error())
			}
			var body io.Reader = content
			if err == storage.ErrNotFound {
				res, err := defaultFetchClient.Fetch(ctxUrl)
				if err != nil {
					return rex.Status(500, "Failed to fetch page html")
				}
				defer res.Body.Close()
				if res.StatusCode != 200 {
					if res.StatusCode == 404 {
						return rex.Status(404, "Page html not found")
					}
					return rex.Status(500, "Failed to fetch page html")
				}
				tokenizer := html.NewTokenizer(io.LimitReader(res.Body, 2*MB))
				configCSS := ""
				content := []string{}
				jsEntries := map[string]struct{}{}
				importMap := common.ImportMap{}
				for {
					tt := tokenizer.Next()
					if tt == html.ErrorToken {
						break
					}
					if tt == html.StartTagToken {
						name, moreAttr := tokenizer.TagName()
						switch string(name) {
						case "style":
							for moreAttr {
								var key, val []byte
								key, val, moreAttr = tokenizer.TagAttr()
								if bytes.Equal(key, []byte("type")) && bytes.Equal(val, []byte("uno/css")) {
									tokenizer.Next()
									innerText := bytes.TrimSpace(tokenizer.Text())
									if len(innerText) > 0 {
										configCSS += string(innerText)
									}
									break
								}
							}
						case "script":
							srcAttr := ""
							mainAttr := ""
							typeAttr := ""
							for moreAttr {
								var key, val []byte
								key, val, moreAttr = tokenizer.TagAttr()
								if bytes.Equal(key, []byte("src")) {
									srcAttr = string(val)
								} else if bytes.Equal(key, []byte("main")) {
									mainAttr = string(val)
								} else if bytes.Equal(key, []byte("type")) {
									typeAttr = string(val)
								}
							}
							if typeAttr == "importmap" {
								tokenizer.Next()
								innerText := bytes.TrimSpace(tokenizer.Text())
								if len(innerText) > 0 {
									err := json.Unmarshal(innerText, &importMap)
									if err == nil {
										importMap.Src = ctxUrl.Path
									}
								}
							} else if srcAttr == "" {
								// inline script content
								tokenizer.Next()
								content = append(content, string(tokenizer.Text()))
							} else {
								if mainAttr != "" && isHttpSepcifier(srcAttr) {
									if !isHttpSepcifier(mainAttr) && endsWith(mainAttr, moduleExts...) {
										jsEntries[mainAttr] = struct{}{}
									}
								} else if !isHttpSepcifier(srcAttr) && endsWith(srcAttr, moduleExts...) {
									jsEntries[srcAttr] = struct{}{}
								}
							}
						case "link", "meta", "title", "base", "head", "noscript", "slot", "template", "option":
							// ignore
						default:
							content = append(content, string(tokenizer.Raw()))
						}
					}
				}
				if configCSS == "" {
					res, err := defaultFetchClient.Fetch(ctxUrl.ResolveReference(&url.URL{Path: "./uno.css"}))
					if err != nil {
						return rex.Status(500, "Failed to lookup config css")
					}
					if res.StatusCode == 404 {
						res.Body.Close()
						res, err = defaultFetchClient.Fetch(ctxUrl.ResolveReference(&url.URL{Path: "/uno.css"}))
						if err != nil {
							return rex.Status(500, "Failed to lookup config css")
						}
					}
					defer res.Body.Close()
					// ignore non-exist config css
					if res.StatusCode != 404 {
						if res.StatusCode != 200 {
							return rex.Status(500, "Failed to fetch config css")
						}
						css, err := io.ReadAll(res.Body)
						if err != nil {
							return rex.Status(500, "Failed to fetch config css")
						}
						configCSS = string(css)
					}
				}
				for src := range jsEntries {
					url := ctxUrl.ResolveReference(&url.URL{Path: src})
					_, _, _, tree, err := bundleHttpModule(npmrc, url.String(), importMap, true)
					if err == nil {
						for _, code := range tree {
							content = append(content, string(code))
						}
					}
				}
				out, err := generateUnoCSS(npmrc, []string{configCSS, strings.Join(content, "\n")})
				if err != nil {
					return rex.Status(500, "Failed to generate uno.css")
				}
				ret := esbuild.Build(esbuild.BuildOptions{
					Stdin: &esbuild.StdinOptions{
						Sourcefile: "uno.css",
						Contents:   out.Code,
						Loader:     esbuild.LoaderCSS,
					},
					Write:            false,
					MinifyWhitespace: config.Minify,
					MinifySyntax:     config.Minify,
					Target:           targets[target],
				})
				if len(ret.Errors) > 0 {
					return rex.Status(500, ret.Errors[0].Text)
				}
				css := ret.OutputFiles[0].Contents
				body = bytes.NewReader(css)
				go esmStorage.Put(savePath, bytes.NewReader(css))
			}
			ctx.SetHeader("Cache-Control", ccImmutable)
			ctx.SetHeader("Content-Type", ctCSS)
			return body // auto closed
		}

		if strings.HasPrefix(pathname, "/http://") || strings.HasPrefix(pathname, "/https://") {
			query := ctx.Query()
			u, err := url.Parse(pathname[1:])
			if err != nil {
				return rex.Status(400, "Invalid URL")
			}
			if u.Scheme != "http" && u.Scheme != "https" {
				return rex.Status(400, "Invalid URL")
			}
			hostname := u.Hostname()
			// disallow localhost or ip address for production
			if !debug {
				if isLocalhost(hostname) || !regexpDomain.MatchString(hostname) || u.Host == ctx.R.Host {
					return rex.Status(400, "Invalid URL")
				}
			}
			extname := path.Ext(u.Path)
			if !(contains(moduleExts, extname) || extname == ".vue" || extname == ".svelte" || extname == ".md" || extname == ".css") {
				return rex.Redirect(u.String(), http.StatusMovedPermanently)
			}
			im := query.Get("im")
			v := query.Get("v")
			if v != "" && (!regexpVersion.MatchString(v) || len(v) > 32) {
				return rex.Status(400, "Invalid Version Param")
			}
			// determine build target by `?target` query or `User-Agent` header
			target := strings.ToLower(query.Get("target"))
			if targets[target] == 0 {
				target = "es2022"
			}
			h := sha1.New()
			h.Write([]byte(u.String()))
			h.Write([]byte(im))
			h.Write([]byte(v))
			h.Write([]byte(target))
			savePath := normalizeSavePath(zoneId, path.Join("modules", hex.EncodeToString(h.Sum(nil))+".mjs"))
			content, _, err := esmStorage.Get(savePath)
			if err != nil && err != storage.ErrNotFound {
				return rex.Status(500, err.Error())
			}
			var body io.Reader = content
			if err == storage.ErrNotFound {
				importMap := common.ImportMap{}
				if len(im) > 0 {
					imPath, err := atobUrl(im)
					if err != nil {
						return rex.Status(400, "Invalid `im` Param")
					}
					imUrl, err := url.Parse(u.Scheme + "://" + u.Host + imPath)
					if err != nil {
						return rex.Status(400, "Invalid `im` Param")
					}
					res, err := defaultFetchClient.Fetch(imUrl)
					if err != nil {
						return rex.Status(500, "Failed to fetch import map")
					}
					defer res.Body.Close()
					if res.StatusCode != 200 {
						return rex.Status(500, "Failed to fetch import map")
					}
					tokenizer := html.NewTokenizer(io.LimitReader(res.Body, 2*MB))
					for {
						tt := tokenizer.Next()
						if tt == html.ErrorToken {
							break
						}
						if tt == html.StartTagToken {
							name, moreAttr := tokenizer.TagName()
							isImportMapScript := false
							if bytes.Equal(name, []byte("script")) {
								for moreAttr {
									var key, val []byte
									key, val, moreAttr = tokenizer.TagAttr()
									if bytes.Equal(key, []byte("type")) && bytes.Equal(val, []byte("importmap")) {
										isImportMapScript = true
										break
									}
								}
							}
							if isImportMapScript {
								tokenizer.Next()
								innerText := bytes.TrimSpace(tokenizer.Text())
								if len(innerText) > 0 {
									err := json.Unmarshal(innerText, &importMap)
									if err != nil {
										return rex.Status(400, "Invalid import map")
									}
									importMap.Src, _ = atobUrl(im)
								}
								break
							}
						}
					}
				}
				if extname == ".md" {
					for _, kind := range []string{"jsx", "svelte", "vue"} {
						if query.Has(kind) {
							u.RawQuery = kind
							break
						}
					}
				}
				js, jsx, css, _, err := bundleHttpModule(npmrc, u.String(), importMap, false)
				if err != nil {
					return rex.Status(500, "Failed to build module: "+err.Error())
				}
				code := string(js)
				if len(css) > 0 {
					code += fmt.Sprintf(`globalThis.document.head.insertAdjacentHTML("beforeend","<style>"+%s+"</style>")`, utils.MustEncodeJSON(string(css)))
				}
				lang := "js"
				if jsx {
					lang = "jsx"
				}
				out, err := transform(&ResolvedTransformOptions{
					TransformOptions: TransformOptions{
						Filename: u.String(),
						Lang:     lang,
						Code:     code,
						Target:   target,
						Minify:   true,
					},
					importMap:     importMap,
					globalVersion: v,
				})
				if err != nil {
					return rex.Status(500, err.Error())
				}
				body = bytes.NewReader([]byte(out.Code))
				go esmStorage.Put(savePath, strings.NewReader(out.Code))
			}
			if extname == ".css" && query.Has("module") {
				css, err := io.ReadAll(body)
				if closer, ok := body.(io.Closer); ok {
					closer.Close()
				}
				if err != nil {
					return rex.Status(500, "Failed to read css")
				}
				body = strings.NewReader(fmt.Sprintf("var style = document.createElement('style');\nstyle.textContent = %s;\ndocument.head.appendChild(style);\nexport default null;", utils.MustEncodeJSON(string(css))))
			}
			ctx.SetHeader("Cache-Control", ccImmutable)
			if extname == ".css" {
				ctx.SetHeader("Content-Type", ctCSS)
			} else {
				ctx.SetHeader("Content-Type", ctJavaScript)
			}
			return body // auto closed
		}

		// check `/*pathname` pattern
		asteriskPrefix := ""
		if strings.HasPrefix(pathname, "/*") {
			asteriskPrefix = "*"
			pathname = "/" + pathname[2:]
		} else if strings.HasPrefix(pathname, "/gh/*") {
			asteriskPrefix = "*"
			pathname = "/gh/" + pathname[5:]
		} else if strings.HasPrefix(pathname, "/github.com/*") {
			asteriskPrefix = "*"
			pathname = "/gh/" + pathname[13:]
		} else if strings.HasPrefix(pathname, "/pr/*") {
			asteriskPrefix = "*"
			pathname = "/pr/" + pathname[5:]
		} else if strings.HasPrefix(pathname, "/pkg.pr.new/*") {
			asteriskPrefix = "*"
			pathname = "/pr/" + pathname[13:]
		}

		esm, extraQuery, isFixedVersion, isBuildPath, err := praseESMPath(npmrc, pathname)
		if err != nil {
			status := 500
			message := err.Error()
			if strings.HasPrefix(message, "invalid") {
				status = 400
			} else if strings.HasSuffix(message, " not found") {
				status = 404
			}
			return rex.Status(status, message)
		}

		pkgAllowed := config.AllowList.IsPackageAllowed(esm.PkgName)
		pkgBanned := config.BanList.IsPackageBanned(esm.PkgName)
		if !pkgAllowed || pkgBanned {
			return rex.Status(403, "forbidden")
		}

		registryPrefix := ""
		if esm.GhPrefix {
			registryPrefix = "/gh"
		} else if esm.PrPrefix {
			registryPrefix = "/pr"
		}

		// redirect `/@types/PKG` to it's main dts file
		if strings.HasPrefix(esm.PkgName, "@types/") && esm.SubBareName == "" {
			info, err := npmrc.getPackageInfo(esm.PkgName, esm.PkgVersion)
			if err != nil {
				return rex.Status(500, err.Error())
			}
			types := "index.d.ts"
			if info.Types != "" {
				types = info.Types
			} else if info.Typings != "" {
				types = info.Typings
			} else if info.Main != "" && strings.HasSuffix(info.Main, ".d.ts") {
				types = info.Main
			}
			return rex.Redirect(fmt.Sprintf("%s/%s@%s%s", cdnOrigin, info.Name, info.Version, utils.NormalizePathname(types)), http.StatusFound)
		}

		// redirect to main css path for CSS packages
		if css := cssPackages[esm.PkgName]; css != "" && esm.SubBareName == "" {
			url := fmt.Sprintf("%s/%s/%s", cdnOrigin, esm.String(), css)
			return rex.Redirect(url, http.StatusFound)
		}

		// store the raw query
		rawQuery := ctx.R.URL.RawQuery

		// support `https://esm.sh/react?dev&target=es2020/jsx-runtime` pattern for jsx transformer
		for _, jsxRuntime := range []string{"/jsx-runtime", "/jsx-dev-runtime"} {
			if strings.HasSuffix(rawQuery, jsxRuntime) {
				if esm.SubPath == "" {
					esm.SubPath = jsxRuntime[1:]
				} else {
					esm.SubPath = esm.SubPath + jsxRuntime
				}
				esm.SubBareName = esm.SubPath
				pathname = fmt.Sprintf("/%s/%s", esm.PkgName, esm.SubPath)
				ctx.R.URL.RawQuery = strings.TrimSuffix(rawQuery, jsxRuntime)
				break
			}
		}

		// apply the extra query if exists
		if extraQuery != "" {
			qs := []string{extraQuery}
			if rawQuery != "" {
				qs = append(qs, rawQuery)
			}
			ctx.R.URL.RawQuery = strings.Join(qs, "&")
		}

		// parse the query
		query := ctx.Query()

		// use `?path=$PATH` query to override the pathname
		if v := query.Get("path"); v != "" {
			esm.SubPath = utils.NormalizePathname(v)[1:]
			esm.SubBareName = toModuleBareName(esm.SubPath, true)
		}

		// check the response type
		resType := ESMEntry
		if esm.SubPath != "" {
			ext := path.Ext(esm.SubPath)
			switch ext {
			case ".js", ".mjs":
				if isBuildPath {
					resType = ESMBuild
				}
			case ".ts", ".mts":
				if endsWith(pathname, ".d.ts", ".d.mts") {
					resType = ESMDts
				}
			case ".css":
				if isBuildPath {
					resType = ESMBuild
				} else {
					resType = RawFile
				}
			case ".map":
				if isBuildPath {
					resType = ESMSourceMap
				} else {
					resType = RawFile
				}
			default:
				if ext != "" && assetExts[ext[1:]] {
					resType = RawFile
				}
			}
		}
		if query.Has("raw") {
			resType = RawFile
		}

		// redirect to the url with fixed package version
		if !isFixedVersion {
			if isBuildPath {
				subPath := ""
				query := ""
				if esm.SubPath != "" {
					subPath = "/" + esm.SubPath
				}
				if rawQuery != "" {
					query = "?" + rawQuery
				}
				ctx.SetHeader("Cache-Control", cc1hour)
				return rex.Redirect(fmt.Sprintf("%s/%s%s%s", cdnOrigin, esm.PackageName(), subPath, query), http.StatusFound)
			}
			if resType != ESMEntry {
				pkgName := esm.PkgName
				pkgVersion := esm.PkgVersion
				subPath := ""
				qs := ""
				if strings.HasPrefix(pkgName, "@jsr/") {
					pkgName = "jsr/@" + strings.ReplaceAll(pkgName[5:], "__", "/")
				}
				if esm.SubPath != "" {
					subPath = "/" + esm.SubPath
					// workaround for es5-ext "../#/.." path
					if esm.PkgName == "es5-ext" {
						subPath = strings.ReplaceAll(subPath, "/#/", "/%23/")
					}
				}
				if extraQuery != "" {
					pkgVersion += "&" + extraQuery
				}
				if rawQuery != "" {
					qs = "?" + rawQuery
				}
				ctx.SetHeader("Cache-Control", cc1hour)
				return rex.Redirect(fmt.Sprintf("%s%s/%s%s@%s%s%s", cdnOrigin, registryPrefix, asteriskPrefix, pkgName, pkgVersion, subPath, qs), http.StatusFound)
			}
		} else {
			// serve `*.wasm` as an es6 module when `?module` query is set (requires `top-level-await` support)
			if resType == RawFile && strings.HasSuffix(esm.SubPath, ".wasm") && query.Has("module") {
				buf := &bytes.Buffer{}
				wasmUrl := cdnOrigin + pathname
				fmt.Fprintf(buf, "/* esm.sh - wasm module */\n")
				fmt.Fprintf(buf, "const data = await fetch(%s).then(r => r.arrayBuffer());\nexport default new WebAssembly.Module(data);", strings.TrimSpace(string(utils.MustEncodeJSON(wasmUrl))))
				ctx.SetHeader("Content-Type", ctJavaScript)
				ctx.SetHeader("Cache-Control", ccImmutable)
				return buf
			}

			// fix url that is related to `import.meta.url`
			if resType == RawFile && isBuildPath && !query.Has("raw") {
				extname := path.Ext(esm.SubPath)
				dir := path.Join(npmrc.StoreDir(), esm.PackageName())
				if !existsDir(dir) {
					_, err := npmrc.installPackage(esm)
					if err != nil {
						return rex.Status(500, err.Error())
					}
				}
				pkgRoot := path.Join(dir, "node_modules", esm.PkgName)
				files, err := findFiles(pkgRoot, "", func(fp string) bool {
					return strings.HasSuffix(fp, extname)
				})
				if err != nil {
					return rex.Status(500, err.Error())
				}
				var file string
				if l := len(files); l == 1 {
					file = files[0]
				} else if l > 1 {
					sort.Sort(sort.Reverse(SortablePaths(files)))
					for _, f := range files {
						if strings.HasSuffix(esm.SubPath, f) {
							file = f
							break
						}
					}
					if file == "" {
						for _, f := range files {
							if path.Base(esm.SubPath) == path.Base(f) {
								file = f
								break
							}
						}
					}
				}
				if file == "" {
					return rex.Status(404, "File not found")
				}
				url := fmt.Sprintf("%s/%s@%s/%s", cdnOrigin, esm.PkgName, esm.PkgVersion, file)
				return rex.Redirect(url, http.StatusMovedPermanently)
			}

			// serve package raw files
			if resType == RawFile {
				var stat storage.Stat
				var content io.ReadCloser
				var cachePath string
				var cacheHit bool
				if config.CacheRawFile {
					cachePath = path.Join("raw", esm.PackageName(), esm.SubPath)
					content, stat, err = esmStorage.Get(cachePath)
					cacheHit = err == nil
				}
				if !cacheHit {
					filename := path.Join(npmrc.StoreDir(), esm.PackageName(), "node_modules", esm.PkgName, esm.SubPath)
					stat, err = os.Lstat(filename)
					if err != nil && os.IsNotExist(err) {
						// if the file not found, try to install the package and retry
						_, err = npmrc.installPackage(esm)
						if err != nil {
							return rex.Status(500, err.Error())
						}
						stat, err = os.Lstat(filename)
					}
					if err != nil {
						if os.IsNotExist(err) {
							return rex.Status(404, "File Not Found")
						}
						return rex.Status(500, err.Error())
					}
					// limit the file size up to 50MB
					if stat.Size() > assetMaxSize {
						return rex.Status(403, "File Too Large")
					}
					content, err = os.Open(filename)
					if err != nil {
						return rex.Status(500, err.Error())
					}
					if config.CacheRawFile {
						go func() {
							f, err := os.Open(filename)
							if err != nil {
								return
							}
							defer f.Close()
							esmStorage.Put(cachePath, f)
						}()
					}
				}
				ctx.SetHeader("Cache-Control", ccImmutable)
				if endsWith(esm.SubPath, ".js", ".mjs", ".jsx") {
					ctx.SetHeader("Content-Type", ctJavaScript)
				} else if endsWith(esm.SubPath, ".ts", ".mts", ".tsx") {
					ctx.SetHeader("Content-Type", ctTypeScript)
				}
				if cacheHit {
					ctx.SetHeader("X-Raw-File-Cache-Status", "HIT")
				}
				contentType := common.ContentType(esm.SubPath)
				if contentType != "" {
					ctx.SetHeader("Content-Type", contentType)
				}
				return rex.Content(esm.SubPath, stat.ModTime(), content) // auto closed
			}

			// serve build/dts files
			if resType == ESMBuild || resType == ESMSourceMap || resType == ESMDts {
				var savePath string
				if resType == ESMDts {
					savePath = path.Join("types", pathname)
				} else {
					savePath = path.Join("builds", pathname)
				}
				savePath = normalizeSavePath(zoneId, savePath)
				content, stat, err := esmStorage.Get(savePath)
				if err != nil {
					if err != storage.ErrNotFound {
						return rex.Status(500, err.Error())
					} else if resType == ESMSourceMap {
						return rex.Status(404, "Not found")
					}
				}
				if err == nil {
					if query.Has("worker") && resType == ESMBuild {
						moduleUrl := cdnOrigin + pathname
						ctx.SetHeader("Content-Type", ctJavaScript)
						ctx.SetHeader("Cache-Control", ccImmutable)
						return fmt.Sprintf(
							`export default function workerFactory(injectOrOptions) { const options = typeof injectOrOptions === "string" ? { inject: injectOrOptions }: injectOrOptions ?? {}; const { inject, name = "%s" } = options; const blob = new Blob(['import * as $module from "%s";', inject].filter(Boolean), { type: "application/javascript" }); return new Worker(URL.createObjectURL(blob), { type: "module", name })}`,
							moduleUrl,
							moduleUrl,
						)
					}
					if resType == ESMDts {
						ctx.SetHeader("Content-Type", ctTypeScript)
					} else if resType == ESMSourceMap {
						ctx.SetHeader("Content-Type", ctJSON)
					} else if strings.HasSuffix(pathname, ".css") {
						ctx.SetHeader("Content-Type", ctCSS)
					} else {
						ctx.SetHeader("Content-Type", ctJavaScript)
					}
					ctx.SetHeader("Cache-Control", ccImmutable)
					if resType == ESMDts {
						defer content.Close()
						buffer, err := io.ReadAll(content)
						if err != nil {
							return rex.Status(500, err.Error())
						}
						return bytes.ReplaceAll(buffer, []byte("{ESM_CDN_ORIGIN}"), []byte(cdnOrigin))
					}
					return rex.Content(savePath, stat.ModTime(), content) // auto closed
				}
			}
		}

		// determine build target by `?target` query or `User-Agent` header
		target := strings.ToLower(query.Get("target"))
		targetFromUA := targets[target] == 0
		if targetFromUA {
			target = getBuildTargetByUA(ctx.UserAgent())
		}

		// redirect to the url with fixed package version for `deno` and `denonext` target
		if !isFixedVersion && (target == "denonext" || target == "deno") {
			pkgName := esm.PkgName
			pkgVersion := esm.PkgVersion
			subPath := ""
			qs := ""
			if strings.HasPrefix(pkgName, "@jsr/") {
				pkgName = "jsr/@" + strings.ReplaceAll(pkgName[5:], "__", "/")
			}
			if esm.SubPath != "" {
				subPath = "/" + esm.SubPath
				// workaround for es5-ext "../#/.." path
				if esm.PkgName == "es5-ext" {
					subPath = strings.ReplaceAll(subPath, "/#/", "/%23/")
				}
			}
			if extraQuery != "" {
				pkgVersion += "&" + extraQuery
			}
			if rawQuery == "target="+target {
				rawQuery = ""
			} else if p := "&target=" + target; strings.Contains(rawQuery, p) {
				rawQuery = strings.ReplaceAll(rawQuery, p, "")
			} else if p := "target=" + target + "&"; strings.Contains(rawQuery, p) {
				rawQuery = strings.ReplaceAll(rawQuery, p, "")
			}
			if rawQuery != "" {
				qs = "?" + rawQuery
			}
			ctx.SetHeader("Cache-Control", cc1hour)
			if targetFromUA {
				appendVaryHeader(ctx.W.Header(), "User-Agent")
			}
			return rex.Redirect(fmt.Sprintf("%s%s/%s%s@%s%s%s", cdnOrigin, registryPrefix, asteriskPrefix, pkgName, pkgVersion, subPath, qs), http.StatusFound)
		}

		// check `?alias` query
		alias := map[string]string{}
		if query.Has("alias") {
			for _, p := range strings.Split(query.Get("alias"), ",") {
				p = strings.TrimSpace(p)
				if p != "" {
					name, to := utils.SplitByFirstByte(p, ':')
					name = strings.TrimSpace(name)
					to = strings.TrimSpace(to)
					if name != "" && to != "" && name != esm.PkgName {
						alias[name] = to
					}
				}
			}
		}

		// check `?deps` query
		deps := map[string]string{}
		if query.Has("deps") {
			for _, v := range strings.Split(query.Get("deps"), ",") {
				v = strings.TrimSpace(v)
				if v != "" {
					m, _, _, _, err := praseESMPath(npmrc, v)
					if err != nil {
						return rex.Status(400, fmt.Sprintf("Invalid deps query: %v not found", v))
					}
					if esm.PkgName == "react-dom" && m.PkgName == "react" {
						// make sure react-dom and react are in the same version
						continue
					}
					if m.PkgName != esm.PkgName {
						deps[m.PkgName] = m.PkgVersion
					}
				}
			}
		}

		// check `?exports` query
		exports := NewStringSet()
		if query.Has("exports") {
			value := query.Get("exports")
			for _, p := range strings.Split(value, ",") {
				p = strings.TrimSpace(p)
				if regexpJSIdent.MatchString(p) {
					exports.Add(p)
				}
			}
		}

		// check `?conditions` query
		var conditions []string
		conditionsSet := NewStringSet()
		if query.Has("conditions") {
			for _, p := range strings.Split(query.Get("conditions"), ",") {
				p = strings.TrimSpace(p)
				if p != "" && !strings.ContainsRune(p, ' ') && !conditionsSet.Has(p) {
					conditionsSet.Add(p)
					conditions = append(conditions, p)
				}
			}
		}

		// check `?external` query
		external := NewStringSet()
		externalAll := asteriskPrefix == "*"
		for _, p := range strings.Split(query.Get("external"), ",") {
			p = strings.TrimSpace(p)
			if p == "*" {
				external.Reset()
				externalAll = true
				break
			}
			if p != "" {
				external.Add(p)
			}
		}

		buildArgs := BuildArgs{
			alias:       alias,
			conditions:  conditions,
			deps:        deps,
			exports:     exports,
			externalAll: externalAll,
			external:    external,
		}

		// match path `PKG@VERSION/X-${args}/esnext/SUBPATH`
		argsX := false
		if resType == ESMBuild || resType == ESMDts {
			a := strings.Split(esm.SubBareName, "/")
			if len(a) > 1 && strings.HasPrefix(a[0], "X-") {
				args, err := decodeBuildArgs(strings.TrimPrefix(a[0], "X-"))
				if err != nil {
					return rex.Status(500, "Invalid build args: "+a[0])
				}
				esm.SubPath = strings.Join(strings.Split(esm.SubPath, "/")[1:], "/")
				esm.SubBareName = toModuleBareName(esm.SubPath, true)
				buildArgs = args
				argsX = true
			}
		}

		// fix the build args that are from the query
		if !argsX {
			err := normalizeBuildArgs(npmrc, path.Join(npmrc.StoreDir(), esm.PackageName()), &buildArgs, esm)
			if err != nil {
				return rex.Status(500, err.Error())
			}
		}

		// build and return `.d.ts`
		if resType == ESMDts {
			readDts := func() (content io.ReadCloser, stat storage.Stat, err error) {
				args := ""
				if a := encodeBuildArgs(buildArgs, true); a != "" {
					args = "X-" + a
				}
				savePath := normalizeSavePath(zoneId, path.Join(fmt.Sprintf(
					"types/%s/%s",
					esm.PackageName(),
					args,
				), esm.SubPath))
				content, stat, err = esmStorage.Get(savePath)
				return
			}
			content, _, err := readDts()
			if err != nil {
				if err != storage.ErrNotFound {
					return rex.Status(500, err.Error())
				}
				buildCtx := NewBuildContext(zoneId, npmrc, esm, buildArgs, "types", false, BundleDefault, false)
				c := buildQueue.Add(buildCtx, ctx.RemoteIP())
				select {
				case output := <-c.C:
					if output.err != nil {
						if output.err.Error() == "types not found" {
							return rex.Status(404, "Types Not Found")
						}
						return rex.Status(500, "types: "+output.err.Error())
					}
				case <-time.After(time.Duration(config.BuildWaitTime) * time.Second):
					ctx.SetHeader("Cache-Control", ccMustRevalidate)
					return rex.Status(http.StatusRequestTimeout, "timeout, we are transforming the types hardly, please try again later!")
				}
				content, _, err = readDts()
			}
			if err != nil {
				if err == storage.ErrNotFound {
					return rex.Status(404, "Types Not Found")
				}
				return rex.Status(500, err.Error())
			}
			defer content.Close()
			buffer, err := io.ReadAll(content)
			if err != nil {
				return rex.Status(500, err.Error())
			}
			ctx.SetHeader("Content-Type", ctTypeScript)
			ctx.SetHeader("Cache-Control", ccImmutable)
			return bytes.ReplaceAll(buffer, []byte("{ESM_CDN_ORIGIN}"), []byte(cdnOrigin))
		}

		if !argsX {
			externalRequire := query.Has("external-require")
			// workaround: force "unocss/preset-icons" to external `require` calls
			if !externalRequire && esm.PkgName == "@unocss/preset-icons" {
				externalRequire = true
			}
			buildArgs.externalRequire = externalRequire
			buildArgs.keepNames = query.Has("keep-names")
			buildArgs.ignoreAnnotations = query.Has("ignore-annotations")
		}

		bundleMode := BundleDefault
		if (query.Has("bundle") && query.Get("bundle") != "false") || query.Has("bundle-all") || query.Has("bundle-deps") || query.Has("standalone") {
			bundleMode = BundleAll
		} else if query.Get("bundle") == "false" || query.Has("no-bundle") {
			bundleMode = BundleFalse
		}

		isDev := query.Has("dev")
		isPkgCss := query.Has("css")
		isWorker := query.Has("worker")
		noDts := query.Has("no-dts") || query.Has("no-check")

		// force react/jsx-dev-runtime and react-refresh into `dev` mode
		if !isDev && ((esm.PkgName == "react" && esm.SubBareName == "jsx-dev-runtime") || esm.PkgName == "react-refresh") {
			isDev = true
		}

		if resType == ESMBuild {
			a := strings.Split(esm.SubBareName, "/")
			if len(a) > 0 {
				maybeTarget := a[0]
				if _, ok := targets[maybeTarget]; ok {
					submodule := strings.Join(a[1:], "/")
					if strings.HasSuffix(submodule, ".bundle") {
						submodule = strings.TrimSuffix(submodule, ".bundle")
						bundleMode = BundleAll
					} else if strings.HasSuffix(submodule, ".nobundle") {
						submodule = strings.TrimSuffix(submodule, ".nobundle")
						bundleMode = BundleFalse
					}
					if strings.HasSuffix(submodule, ".development") {
						submodule = strings.TrimSuffix(submodule, ".development")
						isDev = true
					}
					basename := strings.TrimSuffix(path.Base(esm.PkgName), ".js")
					if strings.HasSuffix(submodule, ".css") && !strings.HasSuffix(esm.SubPath, ".js") {
						if submodule == basename+".css" {
							esm.SubBareName = ""
							target = maybeTarget
						} else {
							url := fmt.Sprintf("%s/%s", cdnOrigin, esm.String())
							return rex.Redirect(url, http.StatusFound)
						}
					} else {
						isMjs := strings.HasSuffix(esm.SubPath, ".mjs")
						if isMjs && submodule == basename {
							submodule = ""
						}
						esm.SubBareName = submodule
						target = maybeTarget
					}
				}
			}
		}
		buildCtx := NewBuildContext(zoneId, npmrc, esm, buildArgs, target, !targetFromUA, bundleMode, isDev)
		ret, err := buildCtx.Query()
		if err != nil {
			return rex.Status(500, err.Error())
		}
		if ret == nil {
			c := buildQueue.Add(buildCtx, ctx.RemoteIP())
			select {
			case output := <-c.C:
				if output.err != nil {
					msg := output.err.Error()
					if strings.Contains(msg, "ERR_PNPM_FETCH_404") {
						return rex.Status(404, "Package or version not found")
					}
					if strings.Contains(msg, "no such file or directory") ||
						strings.Contains(msg, "is not exported from package") {
						// redirect old build path (.js) to new build path (.mjs)
						if strings.HasSuffix(esm.SubPath, "/"+esm.PkgName+".js") {
							url := strings.TrimSuffix(ctx.R.URL.String(), ".js") + ".mjs"
							return rex.Redirect(url, http.StatusFound)
						}
						ctx.SetHeader("Cache-Control", ccImmutable)
						return rex.Status(404, "module not found")
					}
					if strings.HasSuffix(msg, " not found") {
						return rex.Status(404, msg)
					}
					if strings.Contains(msg, "ERR_PNPM") {
						return rex.Status(500, "Failed to install package")
					}
					return rex.Status(500, msg)
				}
				ret = output.result
			case <-time.After(time.Duration(config.BuildWaitTime) * time.Second):
				ctx.SetHeader("Cache-Control", ccMustRevalidate)
				return rex.Status(http.StatusRequestTimeout, "timeout, we are building the package hardly, please try again later!")
			}
		}

		// redirect to `*.d.ts` file
		if ret.TypesOnly {
			dtsUrl := cdnOrigin + ret.Dts
			ctx.SetHeader("X-TypeScript-Types", dtsUrl)
			ctx.SetHeader("Content-Type", ctJavaScript)
			ctx.SetHeader("Cache-Control", ccImmutable)
			if ctx.R.Method == http.MethodHead {
				return []byte{}
			}
			return []byte("export default null;\n")
		}

		// redirect to package css from `?css`
		if isPkgCss && esm.SubBareName == "" {
			if !ret.CSS {
				return rex.Status(404, "Package CSS not found")
			}
			url := fmt.Sprintf("%s%s.css", cdnOrigin, strings.TrimSuffix(buildCtx.Path(), ".mjs"))
			return rex.Redirect(url, 301)
		}

		// if the response type is `ResBuild`, return the build js/css content
		if resType == ESMBuild {
			savePath := buildCtx.getSavepath()
			if strings.HasSuffix(esm.SubPath, ".css") {
				path, _ := utils.SplitByLastByte(savePath, '.')
				savePath = path + ".css"
			}
			f, fi, err := esmStorage.Get(savePath)
			if err != nil {
				if err == storage.ErrNotFound {
					return rex.Status(404, "File not found")
				}
				return rex.Status(500, err.Error())
			}
			ctx.SetHeader("Cache-Control", ccImmutable)
			if endsWith(savePath, ".css") {
				ctx.SetHeader("Content-Type", ctCSS)
			} else if endsWith(savePath, ".mjs", ".js") {
				ctx.SetHeader("Content-Type", ctJavaScript)
				if isWorker {
					f.Close()
					moduleUrl := cdnOrigin + buildCtx.Path()
					return fmt.Sprintf(
						`export default function workerFactory(injectOrOptions) { const options = typeof injectOrOptions === "string" ? { inject: injectOrOptions }: injectOrOptions ?? {}; const { inject, name = "%s" } = options; const blob = new Blob(['import * as $module from "%s";', inject].filter(Boolean), { type: "application/javascript" }); return new Worker(URL.createObjectURL(blob), { type: "module", name })}`,
						moduleUrl,
						moduleUrl,
					)
				}
			}
			return rex.Content(savePath, fi.ModTime(), f) // auto closed
		}

		buf := bytes.NewBuffer(nil)
		fmt.Fprintf(buf, `/* esm.sh - %v */%s`, esm, EOL)

		if isWorker {
			moduleUrl := cdnOrigin + buildCtx.Path()
			fmt.Fprintf(buf,
				`export default function workerFactory(injectOrOptions) { const options = typeof injectOrOptions === "string" ? { inject: injectOrOptions }: injectOrOptions ?? {}; const { inject, name = "%s" } = options; const blob = new Blob(['import * as $module from "%s";', inject].filter(Boolean), { type: "application/javascript" }); return new Worker(URL.createObjectURL(blob), { type: "module", name })}`,
				moduleUrl,
				moduleUrl,
			)
		} else {
			if len(ret.Deps) > 0 {
				for _, dep := range ret.Deps {
					fmt.Fprintf(buf, `import "%s";%s`, dep, EOL)
				}
			}
			ctx.SetHeader("X-ESM-Path", buildCtx.Path())
			fmt.Fprintf(buf, `export * from "%s";%s`, buildCtx.Path(), EOL)
			if (ret.FromCJS || ret.HasDefaultExport) && (exports.Len() == 0 || exports.Has("default")) {
				fmt.Fprintf(buf, `export { default } from "%s";%s`, buildCtx.Path(), EOL)
			}
			if ret.FromCJS && exports.Len() > 0 {
				fmt.Fprintf(buf, `import __cjs_exports$ from "%s";%s`, buildCtx.Path(), EOL)
				fmt.Fprintf(buf, `export const { %s } = __cjs_exports$;%s`, strings.Join(exports.Values(), ", "), EOL)
			}
		}

		if ret.Dts != "" && !noDts && !isWorker {
			dtsUrl := cdnOrigin + ret.Dts
			ctx.SetHeader("X-TypeScript-Types", dtsUrl)
		}
		if targetFromUA {
			appendVaryHeader(ctx.W.Header(), "User-Agent")
		}
		if isFixedVersion {
			ctx.SetHeader("Cache-Control", ccImmutable)
		} else {
			ctx.SetHeader("Cache-Control", cc1hour)
		}
		ctx.SetHeader("Content-Type", ctJavaScript)
		if ctx.R.Method == http.MethodHead {
			return []byte{}
		}
		return buf.Bytes()
	}
}

func errorJS(ctx *rex.Context, message string) any {
	buf := bytes.NewBuffer(nil)
	fmt.Fprintf(buf, "/* esm.sh - error */\n")
	fmt.Fprintf(buf, "throw new Error(%s);\n", strings.TrimSpace(string(utils.MustEncodeJSON(strings.TrimSpace("[esm.sh] "+message)))))
	fmt.Fprintf(buf, "export default null;\n")
	ctx.SetHeader("Content-Type", ctJavaScript)
	ctx.SetHeader("Cache-Control", ccImmutable)
	return buf
}
