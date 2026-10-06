//go:build gate

// The decode-check gate harness, not a test: probes each file named in $DETAILS_GATE_FILES (one path a line) and runs
// the decode check on it with the image's ffmpeg, printing the verdict, where the video's last packet ends and the
// container's length. Built for linux/amd64 and run inside the image, with the library mounted read-only:
//
//	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go test -c -tags gate -o details-gate.test ./internal/stages/details
//	docker run --rm --device /dev/dri/renderD128 --group-add 937 -v "/volume1/media:/volume1/media:ro" \
//	  -v "$PWD:/gate" -e DETAILS_GATE_FILES="$(cat files.txt)" --entrypoint /gate/details-gate.test IMAGE -test.v
package details

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"projectionist/internal/config"
	"projectionist/internal/probe"
	"projectionist/internal/store"
)

func TestDecodeGate(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "gate.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ff := config.FFmpeg{Dir: "/usr/lib/jellyfin-ffmpeg", Device: "/dev/dri/renderD128"}
	prober, err := probe.NewProber(ff.FFprobePath(), st.DB())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, path := range strings.Split(strings.TrimSpace(os.Getenv("DETAILS_GATE_FILES")), "\n") {
		info, err := prober.Probe(ctx, path)
		if err != nil || info.Video == nil {
			t.Errorf("%s: probe: %v (video %v)", filepath.Base(path), err, info.Video != nil)
			continue
		}
		end, _ := videoEnd(ctx, ff.FFprobePath(), path, info.Video.Index, info.Duration)
		start := time.Now()
		clean, detail, err := decodeCheck(ctx, ff, path, info.Video, info.Duration)
		t.Logf("%s: clean=%v detail=%q err=%v | container %.3f s, stated video %.3f s, last video packet %.3f s (0 = not used) | %.1f s",
			filepath.Base(path), clean, detail, err, info.Duration, info.Video.Duration, end, time.Since(start).Seconds())
	}
}
