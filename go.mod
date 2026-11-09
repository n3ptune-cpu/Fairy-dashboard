module github.com/n3ptune-cpu/Fairy-dashboard

go 1.27.3

require (
	github.com/coreos/go-oidc/v3 v3.21.0
	github.com/n3ptune-cpu/Fairy-agent v0.4.4
	golang.org/x/oauth2 v0.37.0
	golang.org/x/sys v0.49.0
	gopkg.in/yaml.v3 v3.0.1
)

require github.com/go-jose/go-jose/v4 v4.1.9 // indirect

replace github.com/n3ptune-cpu/Fairy-agent => /tmp/claude-0/-workspace/fb2c47f8-dc0f-5858-9b7f-5237cf33f93b/scratchpad/fairy-sec/agent/Fairy-agent
