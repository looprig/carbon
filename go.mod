module github.com/looprig/coderig

go 1.26.4

tool (
	github.com/securego/gosec/v2/cmd/gosec
	golang.org/x/vuln/cmd/govulncheck
	honnef.co/go/tools/cmd/staticcheck
)

require (
	github.com/looprig/acp v0.1.1
	github.com/looprig/classifiers v0.1.1
	github.com/looprig/core v0.5.0
	github.com/looprig/foreignloops v0.1.1
	github.com/looprig/fsstore v0.3.0
	github.com/looprig/harness v0.20.0
	github.com/looprig/inference v0.7.0
	github.com/looprig/llm v0.10.0
	github.com/looprig/mcp v0.4.0
	github.com/looprig/sandbox v0.5.1
	github.com/looprig/storage v0.3.0
	github.com/looprig/tools v0.6.0
	github.com/looprig/tui v0.12.1
	golang.org/x/sys v0.47.0
)

require (
	charm.land/bubbles/v2 v2.1.1 // indirect
	charm.land/bubbletea/v2 v2.0.8 // indirect
	charm.land/glamour/v2 v2.0.1 // indirect
	charm.land/lipgloss/v2 v2.0.5 // indirect
	cloud.google.com/go v0.123.0 // indirect
	cloud.google.com/go/auth v0.22.0 // indirect
	cloud.google.com/go/compute/metadata v0.9.0 // indirect
	github.com/BurntSushi/toml v1.6.0 // indirect
	github.com/alecthomas/chroma/v2 v2.27.0 // indirect
	github.com/alecthomas/repr v0.5.4 // indirect
	github.com/anthropics/anthropic-sdk-go v1.61.0 // indirect
	github.com/atotto/clipboard v0.1.4 // indirect
	github.com/aymerick/douceur v0.2.0 // indirect
	github.com/bahlo/generic-list-go v0.2.0 // indirect
	github.com/buger/jsonparser v1.6.1 // indirect
	github.com/ccojocar/zxcvbn-go v1.0.4 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/charmbracelet/colorprofile v0.4.3 // indirect
	github.com/charmbracelet/ultraviolet v0.0.0-20260803092147-8b693049ce2a // indirect
	github.com/charmbracelet/x/ansi v0.11.7 // indirect
	github.com/charmbracelet/x/exp/golden v0.0.0-20260803091719-3755ebad01b1 // indirect
	github.com/charmbracelet/x/exp/slice v0.0.0-20260803091719-3755ebad01b1 // indirect
	github.com/charmbracelet/x/term v0.2.2 // indirect
	github.com/charmbracelet/x/termios v0.1.1 // indirect
	github.com/charmbracelet/x/windows v0.2.2 // indirect
	github.com/clipperhouse/displaywidth v0.11.0 // indirect
	github.com/clipperhouse/uax29/v2 v2.7.0 // indirect
	github.com/dlclark/regexp2/v2 v2.5.2 // indirect
	github.com/felixge/httpsnoop v1.1.0 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/google/go-cmp v0.7.0 // indirect
	github.com/google/go-tdx-guest v0.3.1 // indirect
	github.com/google/jsonschema-go v0.4.3 // indirect
	github.com/google/logger v1.1.2 // indirect
	github.com/google/nftables v0.3.0 // indirect
	github.com/google/s2a-go v0.1.9 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/googleapis/enterprise-certificate-proxy v0.3.20 // indirect
	github.com/googleapis/gax-go/v2 v2.23.0 // indirect
	github.com/gookit/color v1.6.1 // indirect
	github.com/gorilla/css v1.0.1 // indirect
	github.com/gorilla/websocket v1.5.3 // indirect
	github.com/invopop/jsonschema v0.14.0 // indirect
	github.com/landlock-lsm/go-landlock v0.9.0 // indirect
	github.com/lucasb-eyer/go-colorful v1.4.1 // indirect
	github.com/mattn/go-runewidth v0.0.27 // indirect
	github.com/mdlayher/netlink v1.11.2 // indirect
	github.com/mdlayher/socket v0.6.1 // indirect
	github.com/microcosm-cc/bluemonday v1.0.27 // indirect
	github.com/modelcontextprotocol/go-sdk v1.7.0 // indirect
	github.com/muesli/cancelreader v0.2.2 // indirect
	github.com/openai/openai-go/v3 v3.50.0 // indirect
	github.com/pb33f/ordered-map/v2 v2.3.1 // indirect
	github.com/rivo/uniseg v0.4.7 // indirect
	github.com/securego/gosec/v2 v2.28.0 // indirect
	github.com/segmentio/asm v1.2.1 // indirect
	github.com/segmentio/encoding v0.5.4 // indirect
	github.com/standard-webhooks/standard-webhooks/libraries v0.0.1 // indirect
	github.com/tidwall/gjson v1.19.0 // indirect
	github.com/tidwall/match v1.2.0 // indirect
	github.com/tidwall/pretty v1.2.1 // indirect
	github.com/tidwall/sjson v1.2.5 // indirect
	github.com/vishvananda/netns v0.0.5 // indirect
	github.com/xo/terminfo v0.0.0-20220910002029-abceb7e1c41e // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	github.com/yuin/goldmark v1.8.5 // indirect
	github.com/yuin/goldmark-emoji v1.0.6 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.69.0 // indirect
	go.opentelemetry.io/otel v1.45.0 // indirect
	go.opentelemetry.io/otel/metric v1.45.0 // indirect
	go.opentelemetry.io/otel/trace v1.45.0 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	go.yaml.in/yaml/v4 v4.0.0-rc.6 // indirect
	golang.org/x/crypto v0.54.0 // indirect
	golang.org/x/exp/typeparams v0.0.0-20260727155853-b88d891fe743 // indirect
	golang.org/x/mod v0.38.0 // indirect
	golang.org/x/net v0.57.0 // indirect
	golang.org/x/oauth2 v0.36.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/telemetry v0.0.0-20260717140457-bdb89881bb75 // indirect
	golang.org/x/text v0.40.0 // indirect
	golang.org/x/time v0.15.0 // indirect
	golang.org/x/tools v0.48.0 // indirect
	golang.org/x/vuln v1.6.0 // indirect
	google.golang.org/api v0.291.0 // indirect
	google.golang.org/genai v1.66.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260803160001-6ac0973c030d // indirect
	google.golang.org/grpc v1.83.0 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
	honnef.co/go/tools v0.7.0 // indirect
	kernel.org/pub/linux/libs/security/libcap/psx v1.2.78 // indirect
)
