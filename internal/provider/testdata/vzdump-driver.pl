#!/usr/bin/perl
# Test stand-in for vzdump: loads the plug-in as PVE::Storage loads custom plug-ins and calls
# the provider in vzdump's order, with vzdump's checks of each return value. The order and the
# checks follow pve-manager PVE/VZDump.pm and qemu-server src/PVE/VZDump/QemuServer.pm
# (git.proxmox.com, master of 2026-11-20); see docs/reference/provider-plugin.md.
#
#   perl -I <root> -I <stubs> vzdump-driver.pl load <root>
#   perl -I <root> -I <stubs> vzdump-driver.pl job <root> <socket> <vmid>
#
# Output: one line per event ("call <method>", "log <level> <message>", "result ..."), then
# "job ok, guest ok|failed" or "job failed: <error>".
use strict;
use warnings;
no warnings 'once';

use PVE::Storage;
use PVE::Storage::Plugin;
use PVE::BackupProvider::Plugin::Base;

$| = 1;
my ($what, $root, $socket, $vmid) = @ARGV;
my $file = 'PVE/Storage/Custom/ResticPVEPlugin.pm';
my $modname = 'PVE::Storage::Custom::ResticPVEPlugin';

# the loader, as in PVE/Storage.pm
eval {
    require $file;
    die "not derived from PVE::Storage::Plugin\n" if !$modname->isa('PVE::Storage::Plugin');
    die "does not provide an api() method\n" if !$modname->can('api');
    my $version = $modname->api();
    die "implements an API version newer than current ($version > " . PVE::Storage::APIVER . ")\n"
        if $version > PVE::Storage::APIVER;
    my $min = PVE::Storage::APIVER - PVE::Storage::APIAGE;
    die "API version too old, please update the plugin ($version < $min)\n" if $version < $min;
    $modname->register();
    print "loaded api $version\n";
};
if ($@) {
    print "load failed: $@";
    exit 1;
}
exit 0 if $what eq 'load';
die "usage\n" if $what ne 'job';

my $pd = $modname->plugindata();
die "no backup-provider feature\n" if !$pd->{features}->{'backup-provider'};
die "content without backup\n" if !$pd->{content}->[0]->{backup};
die "type\n" if $modname->type() ne 'resticpve';
my @st = $modname->status('restic-pve', {}, {});
die "status not active\n" if !$st[3];

$PVE::BackupProvider::Plugin::ResticPVE::SOCKET = $socket;
my $log = sub { my ($level, $msg) = @_; print "log $level $msg\n" };
# PVE::Storage::new_backup_provider: $plugin->new_backup_provider($scfg, $storeid, $log_function)
my $p = $modname->new_backup_provider({ type => 'resticpve' }, 'restic-pve', $log);
die "provider is not a PVE::BackupProvider::Plugin::Base\n" if !$p->isa('PVE::BackupProvider::Plugin::Base');
for my $m (@PVE::BackupProvider::Plugin::Base::METHODS) {
    die "provider does not implement $m\n" if $p->can($m) == PVE::BackupProvider::Plugin::Base->can($m);
}

my $SAFE = $PVE::Storage::SAFE_CHAR_CLASS_RE;

sub task {
    my $ok = eval {
        print "call backup_init\n";
        my $init = $p->backup_init($vmid, 'qemu', 1763600000);
        die "backup init failed: did not receive a valid result from the backup provider\n"
            if !defined($init) || ref($init) ne 'HASH';
        my $name = $init->{'archive-name'};
        die "backup init failed: did not receive an archive name from backup provider\n"
            if !defined($name) || length($name) == 0;
        die "backup init failed: illegal characters in archive name '$name'\n"
            if $name !~ m!^(${SAFE}|/|:)+$!;
        print "result archive-name $name\n";
        print "result provider_name " . $p->provider_name() . "\n";

        print "call backup_get_mechanism\n";
        my $mech = $p->backup_get_mechanism($vmid, 'qemu');
        die "mechanism '$mech' requested by backup provider is not supported for VMs\n"
            if $mech ne 'file-handle' && $mech ne 'nbd';
        print "result mechanism $mech\n";

        my $devices = { 'drive-scsi0' => { size => 20 * 1024**3 }, 'drive-efidisk0' => { size => 540672 } };
        print "call backup_vm_query_incremental\n";
        my $inc = $p->backup_vm_query_incremental($vmid, $devices);
        for my $d (sort keys %$devices) {
            my $mode = $inc->{$d};
            next if !defined $mode;
            die "invalid incremental mode '$mode' returned by backup provider plugin\n"
                if $mode ne 'new' && $mode ne 'use' && $mode ne 'none';
            print "result incremental $d $mode\n";
        }

        my $volumes = {};
        for my $d (sort keys %$devices) {
            $volumes->{$d} = {
                'bitmap-mode' => ($inc->{$d} // '') eq 'use' ? 'reuse' : 'new',
                size => $devices->{$d}->{size},
                'nbd-path' => "/run/qemu-server/$vmid.backup-access.4242//$vmid-nbd.backup-access",
                'bitmap-name' => 'snapshot-access:restic-pve',
            };
        }
        my $config = "boot: order=scsi0\ncores: 2\nname: web\nscsi0: local:$vmid/vm-$vmid-disk-0.raw,size=20G\n";
        print "call backup_vm\n";
        $p->backup_vm($vmid, $config, $volumes, { 'bandwidth-limit' => 40960 * 1024, 'firewall-config' => "[OPTIONS]\nenable: 1\n" });

        print "call backup_cleanup success\n";
        my $res = $p->backup_cleanup($vmid, 'qemu', 1, {});
        print "result archive-size " . ($res->{stats}->{'archive-size'} // 'none') . "\n";
        1;
    };
    my $err = $@;
    if (!$ok) {
        print "guest failed: $err";
        print "call backup_cleanup failure\n";
        eval { $p->backup_cleanup($vmid, 'qemu', 0, { error => $err }) };
        print "log warn backup cleanup for external provider failed - $@" if $@;
    }
    print "call backup_handle_log_file\n";
    eval { $p->backup_handle_log_file($vmid, "/var/tmp/vzdumptmp4242/qemu-$vmid.log") };
    print "log warn handling backup task log by external provider failed - $@" if $@;
    return $ok;
}

my $guest;
my $ok = eval {
    print "call job_init\n";
    $p->job_init(1763600000);
    $guest = task();
    print "call job_cleanup\n";
    $p->job_cleanup();
    1;
};
if (!$ok) {
    my $err = $@;
    eval { $p->job_cleanup() };
    $err .= "job cleanup for external provider failed - $@" if $@;
    print "job failed: $err";
    exit 0;
}
print "job ok, guest " . ($guest ? "ok" : "failed") . "\n";
