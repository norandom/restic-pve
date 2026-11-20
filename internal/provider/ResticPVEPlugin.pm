# restic-pve plug-in, protocol 1. Written by restic-pve from its embedded copy; do not edit. Removed by restic-pve uninstall.
#
# Proxmox gives the record of changed blocks only to a backup provider plug-in. This file is
# that plug-in, and nothing more: every call of a backup job is passed as one JSON line to the
# restic-pve process that started the job, over a root-only socket, and its answer is
# returned. It reads and writes no file and starts no program. Without that process (a job
# started from the Proxmox screens or Proxmox's own schedule) the job is refused in job_init.

package PVE::BackupProvider::Plugin::ResticPVE;

use strict;
use warnings;

use IO::Socket::UNIX;
use JSON::PP;
use MIME::Base64 qw(encode_base64);

use base qw(PVE::BackupProvider::Plugin::Base);

our $PROTOCOL = 1;
our $SOCKET = '/run/restic-pve/provider.sock';
my $MAX_REPLY = 1 << 20;
my $REFUSED = "restic-pve: this storage takes only backups started by restic-pve on the"
    . " backup machine; use another storage for Proxmox's own backup jobs\n";

sub new {
    my ($class, $storage_plugin, $scfg, $storeid, $log_function) = @_;
    return bless { log => $log_function, json => JSON::PP->new->canonical->utf8 }, $class;
}

sub provider_name { return 'restic-pve' }

# One request line out, one reply line back. A reply {"ok":false,"message":...} dies.
sub call {
    my ($self, $method, $args) = @_;
    my $sock = $self->{sock} or die "restic-pve: no job connection for $method\n";
    my $line = $self->{json}->encode({ method => $method, args => $args }) . "\n";
    local $SIG{PIPE} = 'IGNORE'; # a lost connection is an error of this call, not the end of vzdump
    my $lost = sub { delete $self->{sock}; close($sock); die "restic-pve: connection lost in $method: $_[0]\n" };
    while (length $line) {
        my $n = syswrite($sock, $line);
        $lost->("$!") if !defined $n;
        substr($line, 0, $n) = '';
    }
    my $reply = '';
    while ($reply !~ /\n/) {
        my $n = sysread($sock, $reply, 65536, length $reply);
        $lost->(defined $n ? 'closed' : "$!") if !$n;
        $lost->('reply too long') if length($reply) > $MAX_REPLY;
    }
    my $res = eval { $self->{json}->decode((split /\n/, $reply, 2)[0]) };
    die "restic-pve: unreadable reply to $method\n" if ref($res) ne 'HASH';
    if (defined(my $log = $res->{log})) {
        $log =~ s/[^\x20-\x7e]/?/g;
        $self->{log}->('info', substr($log, 0, 1000));
    }
    die "restic-pve: " . ($res->{message} // "$method refused") . "\n" if !$res->{ok};
    return $res->{result};
}

sub job_init {
    my ($self, $start_time) = @_;
    my $sock = IO::Socket::UNIX->new(Peer => $SOCKET) or die $REFUSED;
    $self->{sock} = $sock;
    eval { $self->call('job_init', { protocol => $PROTOCOL, start_time => 0 + $start_time }) };
    if ($@) {
        close($sock);
        delete $self->{sock};
        die $REFUSED;
    }
    return;
}

sub job_cleanup {
    my ($self) = @_;
    return if !$self->{sock};
    eval { $self->call('job_cleanup', {}) };
    my $err = $@;
    close(delete $self->{sock});
    die $err if $err;
    return;
}

sub backup_init {
    my ($self, $vmid, $vmtype, $start_time) = @_;
    return $self->call('backup_init', { vmid => 0 + $vmid, type => $vmtype, start_time => 0 + $start_time });
}

sub backup_cleanup {
    my ($self, $vmid, $vmtype, $success, $info) = @_;
    my $args = { vmid => 0 + $vmid, type => $vmtype, success => $success ? JSON::PP::true : JSON::PP::false };
    $args->{error} = "$info->{error}" if $info && defined $info->{error};
    return $self->call('backup_cleanup', $args);
}

sub backup_get_mechanism {
    my ($self, $vmid, $vmtype) = @_;
    return $self->call('backup_get_mechanism', { vmid => 0 + $vmid, type => $vmtype });
}

sub backup_handle_log_file {
    my ($self, $vmid, $filename) = @_;
    return $self->call('backup_handle_log_file', { vmid => 0 + $vmid, filename => "$filename" });
}

sub backup_vm_query_incremental {
    my ($self, $vmid, $volumes) = @_;
    my $vols = { map { $_ => { size => 0 + $volumes->{$_}->{size} } } keys %$volumes };
    return $self->call('backup_vm_query_incremental', { vmid => 0 + $vmid, volumes => $vols });
}

sub backup_vm {
    my ($self, $vmid, $guest_config, $volumes, $info) = @_;
    my $vols = {};
    for my $dev (keys %$volumes) {
        my $v = $volumes->{$dev};
        $vols->{$dev} = { map { defined $v->{$_} ? ($_ => "$v->{$_}") : () } qw(bitmap-mode bitmap-name nbd-path) };
        $vols->{$dev}->{size} = 0 + $v->{size};
    }
    my $args = { vmid => 0 + $vmid, config_b64 => b64($guest_config), volumes => $vols,
        bandwidth_limit => 0 + ($info->{'bandwidth-limit'} // 0) };
    $args->{firewall_b64} = b64($info->{'firewall-config'}) if defined $info->{'firewall-config'};
    $self->call('backup_vm', $args);
    return;
}

sub b64 {
    my ($data) = @_;
    utf8::encode($data) if utf8::is_utf8($data);
    return encode_base64($data, '');
}

my $NO_RESTORE = "restic-pve: restore with ./restore.sh on the backup machine\n";
my $NO_LXC = "restic-pve: containers are not backed up through this storage\n";

sub restore_get_mechanism { die $NO_RESTORE }
sub archive_get_guest_config { die $NO_RESTORE }
sub archive_get_firewall_config { die $NO_RESTORE }
sub restore_vm_init { die $NO_RESTORE }
sub restore_vm_cleanup { die $NO_RESTORE }
sub restore_vm_volume_init { die $NO_RESTORE }
sub restore_vm_volume_cleanup { die $NO_RESTORE }
sub restore_container_init { die $NO_RESTORE }
sub restore_container_cleanup { die $NO_RESTORE }
sub backup_container_prepare { die $NO_LXC }
sub backup_container { die $NO_LXC }

package PVE::Storage::Custom::ResticPVEPlugin;

use strict;
use warnings;

use base qw(PVE::Storage::Plugin);

my $APIVER_MIN = 11;    # the first storage API with backup providers
my $APIVER_TESTED = 17; # the highest version this file was checked against
my $NO_VOLUMES = "restic-pve: this storage holds no volumes\n";

sub api {
    my $v = PVE::Storage::APIVER();
    return ($v >= $APIVER_MIN && $v <= $APIVER_TESTED) ? $v : $APIVER_TESTED;
}

sub type { return 'resticpve' }

sub plugindata {
    return {
        content => [{ backup => 1 }, { backup => 1 }],
        features => { 'backup-provider' => 1 },
        'sensitive-properties' => {},
    };
}

sub properties { return {} }

sub options {
    return { nodes => { optional => 1 }, disable => { optional => 1 }, content => { optional => 1 } };
}

sub new_backup_provider {
    my ($class, $scfg, $storeid, $log_function) = @_;
    return PVE::BackupProvider::Plugin::ResticPVE->new($class, $scfg, $storeid, $log_function);
}

sub activate_storage { return 1 }
sub deactivate_storage { return 1 }
sub check_connection { return 1 }
sub status { return (0, 0, 0, 1) }
sub list_volumes { return [] }
sub list_images { return [] }
sub prune_backups { return [] }
sub get_volume_attribute { return }
sub update_volume_attribute { return } # vzdump sets notes or protected from site defaults: no volume, nothing to set
sub volume_has_feature { return 0 }

sub parse_volname { die $NO_VOLUMES }
sub path { die $NO_VOLUMES }
sub alloc_image { die $NO_VOLUMES }
sub free_image { die $NO_VOLUMES }
sub clone_image { die $NO_VOLUMES }
sub create_base { die $NO_VOLUMES }
sub activate_volume { die $NO_VOLUMES }
sub deactivate_volume { die $NO_VOLUMES }
sub volume_size_info { die $NO_VOLUMES }
sub volume_resize { die $NO_VOLUMES }
sub volume_snapshot { die $NO_VOLUMES }
sub volume_snapshot_rollback { die $NO_VOLUMES }
sub volume_snapshot_delete { die $NO_VOLUMES }

1;
