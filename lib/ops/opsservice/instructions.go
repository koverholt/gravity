/*
Copyright 2018 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package opsservice

import (
	"bytes"
	"fmt"
	"html/template"
	"net/url"
	"strings"

	"github.com/gravitational/gravity/lib/constants"
	"github.com/gravitational/gravity/lib/defaults"
	"github.com/gravitational/gravity/lib/ops"
	"github.com/gravitational/gravity/lib/schema"
	"github.com/gravitational/gravity/lib/storage"

	"github.com/gravitational/trace"
)

var (
	// gravityTemplateSource is a bash script that downloads gravity binary
	// from an Ops Center and installs it into /usr/bin
	gravityTemplateSource = `
#!/bin/bash
set -e

CURL_OPTS="--retry 100 --retry-delay 0 --connect-timeout 10 --max-time 300 --tlsv1.2 --silent --show-error"
echo "$(date) [INFO] Downloading install agent..."
curl $CURL_OPTS {{if .devmode}}--insecure{{end}} -H "Authorization: Bearer {{.ops_token}}" {{.gravity_url}} -o {{.gravity_bin_path}}
chmod 755 {{.gravity_bin_path}}

echo "$(date) [INFO] Install agent will be using ${TMPDIR:-/tmp} for temporary files"
`

	// installTemplate is a template for instructions to run on nodes during
	// Ops Center initiated installation
	installTemplate = template.Must(
		template.New("instructions").Parse(fmt.Sprintf(`
%v
{{.service_user_env}}={{.service_uid}} \
{{.service_group_env}}={{.service_gid}} \
{{.gravity_bin_path}} {{if .devmode}}--insecure{{end}} --debug install \
    --advertise-addr={{.advertise_addr}} \
    --token={{.install_token}} \
    --cluster={{.cluster_name}} \
    --app={{.app}} \
    --role={{.profile}} \
    --mode={{.mode}} \
    --cloud-provider={{.cloud_provider}} \
    --operation-id={{.operation_id}} \
    --ops-url={{.ops_url}} \
    --ops-token={{.ops_token}} \
    --ops-sni-host={{.ops_sni_host}} {{if .gce_node_tags}}--gce-node-tags={{.gce_node_tags}} {{end}}\
    --ops-tunnel-token={{.ops_tunnel_token}} {{if .background}}1>/dev/null 2>&1 &{{end}}
`, gravityTemplateSource)))

	// joinTemplate is a template for instructions to run on nodes during
	// wizard installation or expand
	joinTemplate = template.Must(
		template.New("instructions").Parse(fmt.Sprintf(`
%v
{{.service_user_env}}={{.service_uid}} \
{{.service_group_env}}={{.service_gid}} \
{{.gravity_bin_path}} {{if .devmode}}--insecure{{end}} --debug join {{.ops_url}} \
    --token={{.install_token}} \
    --advertise-addr={{.advertise_addr}} \
    --server-addr={{.agent_server_addr}} \
    --role={{.profile}} \
    --cloud-provider={{.cloud_provider}} \
    --existing-operation {{if .background}}1>/dev/null 2>&1 &{{end}}
`, gravityTemplateSource)))

	downloadInstructionsTemplate = template.Must(
		template.New("instructions").Parse(`
curl -s --tlsv1.2 {{if .devmode}}--insecure{{end}} "{{.url}}" | sudo bash
`))
)

// getDownloadInstructions returns a command that downloads agents instructions
func (s *site) getDownloadInstructions(token, serverProfile string) (string, error) {
	targetURL := strings.Join([]string{s.packages().PortalURL(), "t", token, serverProfile}, "/")
	url, err := url.ParseRequestURI(targetURL)
	if err != nil {
		return "", trace.Wrap(err)
	}
	var out bytes.Buffer
	err = downloadInstructionsTemplate.Execute(&out, map[string]interface{}{
		"devmode": s.shouldUseInsecure(),
		"url":     url.String(),
	})
	if err != nil {
		return "", trace.Wrap(err)
	}
	return out.String(), nil
}

// getInstallInstructions returns a bash script source that starts agents
// for an Ops Center initiated installation
func (s *site) getInstallInstructions(token storage.ProvisioningToken, serverProfile string, params url.Values) (string, error) {
	tunnelToken, err := s.service.GetTrustedClusterToken(ops.SiteKey{
		AccountID:  token.AccountID,
		SiteDomain: token.SiteDomain,
	})
	if err != nil {
		return "", trace.Wrap(err)
	}
	agentToken, err := s.service.GetClusterAgent(ops.ClusterAgentRequest{
		AccountID:   token.AccountID,
		ClusterName: token.SiteDomain,
	})
	if err != nil {
		return "", trace.Wrap(err)
	}
	vars := map[string]interface{}{
		"devmode":           s.shouldUseInsecure(),
		"service_uid":       s.uid(),
		"service_gid":       s.gid(),
		"gravity_url":       s.packages().PackageDownloadURL(s.gravityPackage),
		"advertise_addr":    params.Get(schema.AdvertiseAddr),
		"install_token":     token.Token,
		"cluster_name":      token.SiteDomain,
		"app":               s.app.Package.String(),
		"profile":           serverProfile,
		"mode":              constants.InstallModeOpsCenter,
		"operation_id":      token.OperationID,
		"ops_url":           s.packages().PortalURL(),
		"ops_token":         agentToken.Password,
		"ops_tunnel_token":  tunnelToken.GetName(),
		"ops_sni_host":      s.service.cfg.SNIHost,
		"background":        params.Get("bg") == "true",
		"service_user_env":  constants.ServiceUserEnvVar,
		"service_group_env": constants.ServiceGroupEnvVar,
		"gravity_bin_path":  defaults.GravityBin,
		"gce_node_tags":     s.gceNodeTags(),
		"cloud_provider":    s.provider,
	}
	var out bytes.Buffer
	err = installTemplate.Execute(&out, vars)
	if err != nil {
		return "", trace.Wrap(err)
	}
	return out.String(), nil
}

// getJoinInstructions returns a bash script source that starts agents for
// a wizard installation or expand
func (s *site) getJoinInstructions(token storage.ProvisioningToken, serverProfile string, params url.Values) (string, error) {
	agentToken, err := s.service.GetClusterAgent(ops.ClusterAgentRequest{
		AccountID:   token.AccountID,
		ClusterName: token.SiteDomain,
	})
	if err != nil {
		return "", trace.Wrap(err)
	}
	vars := map[string]interface{}{
		"devmode":           s.shouldUseInsecure(),
		"service_uid":       s.uid(),
		"service_gid":       s.gid(),
		"gravity_url":       s.packages().PackageDownloadURL(s.gravityPackage),
		"advertise_addr":    params.Get(schema.AdvertiseAddr),
		"install_token":     token.Token,
		"profile":           serverProfile,
		"ops_url":           s.packages().PortalURL(),
		"ops_token":         agentToken.Password,
		"agent_server_addr": s.service.cfg.Agents.ServerAddr(),
		"background":        params.Get("bg") == "true",
		"service_user_env":  constants.ServiceUserEnvVar,
		"service_group_env": constants.ServiceGroupEnvVar,
		"gravity_bin_path":  defaults.GravityBin,
		"cloud_provider":    s.provider,
	}
	var out bytes.Buffer
	err = joinTemplate.Execute(&out, vars)
	if err != nil {
		return "", trace.Wrap(err)
	}
	return out.String(), nil
}
