package tiles

import (
	"io"
	"log"
	"net/http"
	"os"
	"regexp"

	"../common"
)

var xyzRegex *regexp.Regexp = regexp.MustCompile(`^\/tiles\/(\d+)\/(\d+)\/(\d+)\.png$`)

func GetHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, http.StatusText(405), 405)
		return
	}

	if !xyzRegex.MatchString(r.URL.Path) {
		http.Error(w, "wrong format", 400)
		return
	}
	xyz := xyzRegex.FindStringSubmatch(r.URL.Path)
	xyzPath := "downloaded_tiles/" + xyz[1] + "/" + xyz[2] + "/"
	xyzFile := xyzPath + xyz[3] + ".png"

	if _, err := os.Stat(xyzFile); os.IsNotExist(err) {
		resp, err := http.Get("http://a.tile.thunderforest.com/transport/" + xyz[1] + "/" + xyz[2] + "/" + xyz[3] + ".png")
		if err != nil {
			common.LogErr(err)
			return
		}
		defer resp.Body.Close()
		err = os.MkdirAll(xyzPath, 0755)
		if err != nil {
			common.LogErr(err)
			return
		}
		out, err := os.Create(xyzFile)
		if err != nil {
			common.LogErr(err)
			return
		}
		io.Copy(out, resp.Body)
		out.Close()
		log.Println("downloaded " + xyzFile)
	}

	w.Header().Set("Cache-Control", "max-age=86400")
	http.ServeFile(w, r, xyzFile)
}
