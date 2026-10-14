# frozen_string_literal: true

# ruby test/routes_test.rb — exercises ./routes against an in-memory fake of
# the Cloudflare API (two tunnels), a scratch routes.yml and a scratch Caddyfile.
require "minitest/autorun"
require "stringio"
require "tmpdir"

DIR = Dir.mktmpdir("routes-test")
ENV["ROUTES_FILE"] = File.join(DIR, "routes.yml")
ENV["ROUTES_CADDYFILE"] = File.join(DIR, "Caddyfile")
load File.expand_path("../routes", __dir__)

# Tests pick the machine they run on.
module LocalTunnel
  def running_tunnel_id = $local_tunnel
end
Routes::CLI.prepend(LocalTunnel)

class FakeApi
  attr_reader :configs, :records, :calls

  def initialize(configs:, records: [])
    @configs = configs
    @records = records
    @calls = []
  end

  def accounts = []
  def tunnels(_) = [
    { "id" => "mini", "name" => "macmini", "config_src" => "cloudflare" },
    { "id" => "pc", "name" => "pc", "config_src" => "cloudflare" },
    { "id" => "old", "name" => "legacy", "config_src" => "local" },
  ]
  def tunnel_config(_, id) = Marshal.load(Marshal.dump(@configs[id]))
  def zones(_) = [
    { "id" => "z1", "name" => "grillermo.com", "account" => { "id" => "acct" } },
    { "id" => "z2", "name" => "chiq.me", "account" => { "id" => "acct" } },
  ]
  def dns_records(zone_id) = @records.select { |r| r["zone"] == zone_id }

  def put_tunnel_config(_, id, config)
    @calls << [:put, id]
    @configs[id] = config
  end

  def create_dns_record(zone_id, record)
    @calls << [:create, record["name"]]
    @records << record.merge("zone" => zone_id, "id" => "r#{@records.length}")
  end

  def update_dns_record(_, id, changes)
    @calls << [:update, id]
    @records.find { |r| r["id"] == id }.merge!(changes)
  end

  def delete_dns_record(_, id)
    @calls << [:delete, id]
    @records.reject! { |r| r["id"] == id }
  end
end

class RoutesTest < Minitest::Test
  MINI = "mini.cfargotunnel.com"
  PC = "pc.cfargotunnel.com"

  def setup
    FileUtils.rm_f(Dir[File.join(DIR, "*")])
    File.write(ENV["ROUTES_CADDYFILE"], ":3020 {\n\treverse_proxy localhost:3021\n}\n")
    $local_tunnel = "mini"
    @api = FakeApi.new(
      configs: {
        "mini" => { "ingress" => [
          { "hostname" => "cal.grillermo.com", "service" => "http://localhost:3020", "originRequest" => {} },
          { "hostname" => "rulinky.grillermo.com", "service" => "http://localhost:3500", "originRequest" => { "noTLSVerify" => true } },
          { "service" => "http_status:404" },
        ], "warp-routing" => { "enabled" => false } },
        "pc" => { "ingress" => [
          { "hostname" => "enhance.chiq.me", "service" => "http://localhost:7000" },
          { "service" => "http_status:404" },
        ] },
      },
      records: [
        { "zone" => "z1", "id" => "a", "type" => "CNAME", "name" => "cal.grillermo.com", "content" => MINI },
        { "zone" => "z1", "id" => "b", "type" => "CNAME", "name" => "rulinky.grillermo.com", "content" => MINI },
        { "zone" => "z2", "id" => "e", "type" => "CNAME", "name" => "enhance.chiq.me", "content" => PC },
        { "zone" => "z1", "id" => "c", "type" => "A", "name" => "taken.grillermo.com", "content" => "1.2.3.4" },
      ],
    )
    @reloads = 0
    @reload_ok = true
    run!("pull")
    mark_caddy
  end

  def mark_caddy
    File.write(ENV["ROUTES_FILE"], File.read(ENV["ROUTES_FILE"]).sub("    id: mini\n", "    id: mini\n    caddy: true\n"))
  end

  def run!(*argv)
    out = StringIO.new
    status = Routes::CLI.new(argv, api: @api, out: out, reload: -> { @reloads += 1; @reload_ok }).run
    assert_equal 0, status
    out.string
  end

  def registry = Routes::Registry.load(ENV["ROUTES_FILE"])
  def caddyfile = File.read(ENV["ROUTES_CADDYFILE"])
  def hosts(id) = @api.configs[id]["ingress"].map { |r| r["hostname"] }
  def record(name) = @api.records.find { |r| r["name"] == name }

  def test_pull_writes_every_dashboard_tunnel_and_sync_is_then_a_no_op
    yml = File.read(ENV["ROUTES_FILE"])
    assert_includes yml, "  macmini:\n    id: mini\n    caddy: true\n    routes:\n      cal.grillermo.com: http://localhost:3020\n"
    assert_includes yml, "        originRequest:\n          noTLSVerify: true\n"
    assert_includes yml, "  pc:\n    id: pc\n    routes:\n      enhance.chiq.me: http://localhost:7000\n"
    refute_includes yml, "legacy"
    assert_includes run!("sync"), "Everything already matches"
    assert_empty @api.calls
  end

  def test_create_defaults_to_the_local_tunnel_and_keeps_other_config
    out = run!("create", "new.chiq.me", "8081")
    assert_includes out, "+ route  new.chiq.me -> http://localhost:8081  (macmini)"
    assert_equal ["cal.grillermo.com", "rulinky.grillermo.com", "new.chiq.me", nil], hosts("mini")
    assert_equal({ "enabled" => false }, @api.configs["mini"]["warp-routing"])
    assert_equal({ "noTLSVerify" => true }, @api.configs["mini"]["ingress"][1]["originRequest"])
    assert_equal MINI, record("new.chiq.me")["content"]
    assert_equal "z2", record("new.chiq.me")["zone"]
    assert_equal [[:put, "mini"], [:create, "new.chiq.me"]], @api.calls
  end

  def test_create_on_another_tunnel
    run!("create", "gpu.chiq.me", "9000", "--tunnel", "pc")
    assert_includes hosts("pc"), "gpu.chiq.me"
    assert_equal PC, record("gpu.chiq.me")["content"]
  end

  def test_create_without_a_local_tunnel_needs_tunnel_flag
    $local_tunnel = nil
    error = assert_raises(Routes::Error) { run!("create", "gpu.chiq.me", "9000") }
    assert_match(/pass --tunnel NAME \(macmini, pc\)/, error.message)
  end

  def test_hostname_belongs_to_one_tunnel
    error = assert_raises(Routes::Error) { run!("create", "enhance.chiq.me", "9000") }
    assert_match(/already routed on pc/, error.message)
  end

  def test_modify_tunnel_moves_route_and_repoints_cname
    out = run!("modify", "rulinky.grillermo.com", "3600", "--tunnel", "pc")
    assert_includes out, "~ dns    rulinky.grillermo.com CNAME macmini => pc"
    assert_equal ["cal.grillermo.com", nil], hosts("mini")
    assert_includes hosts("pc"), "rulinky.grillermo.com"
    assert_equal PC, record("rulinky.grillermo.com")["content"]
    assert_includes @api.calls, [:update, "b"]
  end

  def test_dry_run_changes_nothing
    run!("create", "new.chiq.me", "8081", "--dry-run")
    assert_empty @api.calls
    assert_nil registry.owner("new.chiq.me")
  end

  def test_create_with_caddy_writes_block_and_reloads
    run!("create", "sub.grillermo.com", "http://localhost:8081", "--caddy", "8080")
    assert_equal({ "service" => "http://localhost:8080", "caddy" => "http://localhost:8081" }, registry.tunnel("macmini").routes["sub.grillermo.com"])
    assert_includes caddyfile, ":8080 {\n\treverse_proxy http://localhost:8081\n}\n# routes:end sub.grillermo.com\n"
    assert_equal 1, @reloads

    run!("modify", "sub.grillermo.com", "8082")
    assert_includes caddyfile, "reverse_proxy http://localhost:8082"
    assert_equal 2, @reloads

    run!("delete", "sub.grillermo.com")
    assert_equal ":3020 {\n\treverse_proxy localhost:3021\n}\n", caddyfile
    refute_includes hosts("mini"), "sub.grillermo.com"
    assert_nil record("sub.grillermo.com")
  end

  def test_caddy_only_on_the_caddy_tunnel
    error = assert_raises(Routes::Error) { run!("create", "gpu.chiq.me", "9001", "--caddy", "9000", "--tunnel", "pc") }
    assert_match(/caddy: needs the tunnel marked caddy: true/, error.message)
    assert_empty @api.calls
  end

  def test_caddy_changes_refused_off_the_caddy_machine
    $local_tunnel = "pc"
    error = assert_raises(Routes::Error) { run!("create", "sub.grillermo.com", "8081", "--caddy", "8080", "--tunnel", "macmini") }
    assert_match(/run it on the machine that runs the macmini tunnel/, error.message)
    assert_nil registry.owner("sub.grillermo.com")
    assert_empty @api.calls
  end

  def test_caddy_port_clash_is_rejected_before_anything_changes
    error = assert_raises(Routes::Error) { run!("create", "sub.grillermo.com", "8081", "--caddy", "3020") }
    assert_match(/:3020 is already used/, error.message)
    assert_nil registry.owner("sub.grillermo.com")
    assert_empty @api.calls
  end

  def test_failed_reload_restores_caddyfile_and_skips_cloudflare
    before = caddyfile
    @reload_ok = false
    assert_raises(Routes::Error) { run!("create", "sub.grillermo.com", "8081", "--caddy", "8080") }
    assert_equal before, caddyfile
    assert_empty @api.calls
  end

  def test_modify_changes_only_the_route
    out = run!("modify", "rulinky.grillermo.com", "http://localhost:3600")
    assert_includes out, "~ route  rulinky.grillermo.com"
    assert_equal "http://localhost:3600", @api.configs["mini"]["ingress"][1]["service"]
    assert_equal [[:put, "mini"]], @api.calls
  end

  def test_remove_deletes_route_and_cname
    run!("remove", "rulinky.grillermo.com")
    assert_equal ["cal.grillermo.com", nil], hosts("mini")
    assert_includes @api.calls, [:delete, "b"]
  end

  def test_existing_non_tunnel_record_is_not_clobbered
    error = assert_raises(Routes::Error) { run!("create", "taken.grillermo.com", "9000") }
    assert_match(/already has DNS A 1.2.3.4/, error.message)
    assert_empty @api.calls
  end

  def test_unknown_zone_syncs_route_but_skips_dns
    out = run!("create", "x.antesis.com.mx", "9000")
    assert_includes out, "! dns    x.antesis.com.mx: skipped"
    assert_includes hosts("mini"), "x.antesis.com.mx"
    assert_equal [:put], @api.calls.map(&:first)
  end

  def test_orphan_cname_to_a_managed_tunnel_is_deleted
    @api.records << { "zone" => "z2", "id" => "o", "type" => "CNAME", "name" => "old.chiq.me", "content" => PC }
    assert_includes run!("sync"), "- dns    old.chiq.me CNAME pc"
    assert_equal [[:delete, "o"]], @api.calls
  end

  def test_hand_edited_yaml_is_synced
    File.write(ENV["ROUTES_FILE"], File.read(ENV["ROUTES_FILE"]).sub("3020", "3030"))
    run!("sync")
    assert_equal "http://localhost:3030", @api.configs["mini"]["ingress"][0]["service"]
  end

  def test_pull_keeps_caddy_flag_route_caddy_and_note
    run!("create", "sub.grillermo.com", "8081", "--caddy", "8080")
    File.write(ENV["ROUTES_FILE"], File.read(ENV["ROUTES_FILE"]).sub("\n        caddy: http://localhost:8081\n", "\n        caddy: http://localhost:8081\n        note: my thing\n"))
    run!("pull")
    assert registry.tunnel("macmini").caddy
    assert_equal({ "service" => "http://localhost:8080", "caddy" => "http://localhost:8081", "note" => "my thing" }, registry.tunnel("macmini").routes["sub.grillermo.com"])
  end

  def test_pull_settles_a_host_routed_on_two_tunnels_by_its_dns
    @api.configs["pc"]["ingress"].unshift({ "hostname" => "cal.grillermo.com", "service" => "http://localhost:80" })
    out = run!("pull")
    assert_includes out, "cal.grillermo.com is routed on macmini and pc; kept on macmini (its DNS points there)"
    assert_equal "macmini", registry.owner("cal.grillermo.com").name
    assert_includes run!("sync"), "- route  cal.grillermo.com -> http://localhost:80  (pc)"
    assert_equal ["enhance.chiq.me", nil], hosts("pc")
  end
end
