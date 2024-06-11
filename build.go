package builder

import (
	"context"
	"fmt"
	"strings"

	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/client/llb/sourceresolver"
	"github.com/moby/buildkit/exporter/containerimage/exptypes"
	"github.com/moby/buildkit/frontend/gateway/client"
	"github.com/moby/buildkit/solver/pb"
	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"
)

const (
	keyStack          = "stack"
	LocalNameContext  = "context"
	buildArgPrefix    = "build-arg:"
	keyBuildpackOrder = "buildpackOrder"
	keySkipDetect     = "skipDetect"
)

func Build(ctx context.Context, c client.Client) (*client.Result, error) {
	opts := c.BuildOpts().Opts

	// also accept build args from Moby
	for k, v := range opts {
		if strings.HasPrefix(k, buildArgPrefix) {
			opts[strings.TrimPrefix(k, buildArgPrefix)] = v
		}
	}

	stack := "cflinuxfs2"
	if v, ok := opts[keyStack]; ok {
		stack = v
	}

	buildName, runName, err := builderImageName(stack)
	if err != nil {
		return nil, err
	}

	m, err := readManifest(ctx, c)
	if err != nil {
		return nil, err
	}

	buildpackOrder := ""
	if v, ok := opts[keyBuildpackOrder]; ok {
		buildpackOrder = v
	}

	var env map[string]string
	var startCommand string

	if m != nil {
		if len(m.Applications) > 0 {
			// TODO: allow setting app with target
			app := m.Applications[0]
			env = app.EnvironmentVariables
			startCommand = app.Command
			if app.Buildpack != "" && buildpackOrder == "" {
				buildpackOrder = app.Buildpack
			}
		} else {
			env = m.EnvironmentVariables
			startCommand = m.Command
			if m.Buildpack != "" && buildpackOrder == "" {
				buildpackOrder = m.Buildpack
			}
		}
	}

	// TODO: read buildpacks and download directly

	// TODO: git/http sources
	src := llb.Local(LocalNameContext, llb.SessionID(c.BuildOpts().SessionID), llb.SharedKeyHint("pack-src"))

	builderImage := llb.Image(buildName, llb.WithMetaResolver(c))

	for k, v := range env {
		builderImage = builderImage.AddEnv(k, v)
	}

	skipDetect := "false"
	if v, ok := opts[keySkipDetect]; ok {
		skipDetect = v
	}
	if buildpackOrder != "" {
		buildpackOrder = "-buildpackOrder=" + buildpackOrder
	}

	build := runBuilder(c, builderImage, fmt.Sprintf(`/packs/builder -buildpacksDir /var/lib/buildpacks  -outputDroplet /out/droplet.tgz -outputMetadata /out/result.json -skipDetect=%s %s`, skipDetect, buildpackOrder), llb.Dir("/workspace"))
	build.AddMount("/workspace", src, llb.Readonly)
	build.AddMount("/tmp", llb.Scratch(), llb.AsPersistentCacheDir("buildpack-build-cache", llb.CacheMountShared))

	setupStartCommand := ""
	if startCommand != "" {
		setupStartCommand = fmt.Sprintf("if [ ! -f /out/home/vcap/app/Procfile ] && [ -f /out/home/vcap/staging_info.yml ]; then cat /out/home/vcap/staging_info.yml | jq '.start_command = \\\"%s\\\"' > /out/home/vcap/staging_info.yml.new;  mv /out/home/vcap/staging_info.yml.new /out/home/vcap/staging_info.yml; fi;", startCommand) // staging_info.yml is json !
	}

	extract := llb.Image("alpine").Run(llb.Shlex("apk add --no-cache jq")).Run(llb.Shlex(`ash -c "set -x;mkdir -p /out/home/vcap && tar -C /out/home/vcap -xzf /in/droplet.tgz;`+setupStartCommand+`chown -R 2000:2000 /out/home/vcap"`), llb.WithCustomName("copy droplet to stack"), llb.Dir("/in"))

	extract.AddMount("/in", build.Root(), llb.SourcePath("out"), llb.Readonly)
	st := extract.AddMount("/out", llb.Image(runName))

	def, err := st.Marshal(ctx, llb.WithCaps(c.BuildOpts().LLBCaps))
	if err != nil {
		return nil, err
	}

	eg, ctx := errgroup.WithContext(ctx)

	var res *client.Result
	eg.Go(func() error {
		r, err := c.Solve(ctx, client.SolveRequest{
			Definition: def.ToPB(),
		})
		if err != nil {
			return err
		}
		res = r
		return nil
	})

	var config []byte
	eg.Go(func() error {
		_, _, c, err := c.ResolveImageConfig(ctx, runName, sourceresolver.Opt{})
		if err != nil {
			return err
		}
		config = c
		return nil
	})

	if err := eg.Wait(); err != nil {
		return nil, err
	}
	// TODO: is the build label needed?
	res.AddMeta(exptypes.ExporterImageConfigKey, config)

	return res, nil
}

func runBuilder(c client.Client, img llb.State, cmd string, opts ...llb.RunOption) llb.ExecState {
	// work around docker 18.06 executor with no cgroups mounted because build has
	// a hard requirement on the file

	caps := c.BuildOpts().LLBCaps

	mountCgroups := (&caps).Supports(pb.CapExecCgroupsMounted) != nil

	opts = append(opts, llb.WithCustomName(cmd))

	if mountCgroups {
		cmd = `sh -c "mkdir -p /sys/fs/cgroup/memory && echo 9223372036854771712 > /sys/fs/cgroup/memory/memory.limit_in_bytes && ` + cmd + `"`
	}

	es := img.Run(append(opts, llb.Shlex(cmd))...)

	if mountCgroups {
		es.AddMount("/sys/fs/cgroup", llb.Scratch())
		alpine := llb.Image("alpine").Run(llb.Shlex(`sh -c 'echo "127.0.0.1 $(hostname)" > /out/hosts'`), llb.WithCustomName("[internal] make hostname resolvable"))
		hosts := alpine.AddMount("/out", llb.Scratch())
		es.AddMount("/etc/hosts", hosts, llb.SourcePath("hosts"), llb.Readonly)
	}

	return es
}

func builderImageName(stack string) (string, string, error) {
	switch stack {
	case "cflinuxfs2":
		return "docker.io/packs/cflinuxfs2:build", "docker.io/packs/cflinuxfs2:run", nil
	default:
		return "", "", errors.Errorf("unsupported stack %s", stack)
	}
}
