# Test stub of PVE::BackupProvider::Plugin::Base (pve-storage): the 21 methods of the real
# class (pve-storage src/PVE/BackupProvider/Plugin/Base.pm), each dying as the real one does until a subclass implements it.
package PVE::BackupProvider::Plugin::Base;

use strict;
use warnings;

our @METHODS = qw(new provider_name job_init job_cleanup backup_init backup_cleanup backup_get_mechanism backup_handle_log_file backup_vm_query_incremental backup_vm backup_container_prepare backup_container restore_get_mechanism archive_get_guest_config archive_get_firewall_config restore_vm_init restore_vm_cleanup restore_vm_volume_init restore_vm_volume_cleanup restore_container_init restore_container_cleanup);

sub new { die "implement me in subclass" }
sub provider_name { die "implement me in subclass" }
sub job_init { die "implement me in subclass" }
sub job_cleanup { die "implement me in subclass" }
sub backup_init { die "implement me in subclass" }
sub backup_cleanup { die "implement me in subclass" }
sub backup_get_mechanism { die "implement me in subclass" }
sub backup_handle_log_file { die "implement me in subclass" }
sub backup_vm_query_incremental { die "implement me in subclass" }
sub backup_vm { die "implement me in subclass" }
sub backup_container_prepare { die "implement me in subclass" }
sub backup_container { die "implement me in subclass" }
sub restore_get_mechanism { die "implement me in subclass" }
sub archive_get_guest_config { die "implement me in subclass" }
sub archive_get_firewall_config { die "implement me in subclass" }
sub restore_vm_init { die "implement me in subclass" }
sub restore_vm_cleanup { die "implement me in subclass" }
sub restore_vm_volume_init { die "implement me in subclass" }
sub restore_vm_volume_cleanup { die "implement me in subclass" }
sub restore_container_init { die "implement me in subclass" }
sub restore_container_cleanup { die "implement me in subclass" }

1;
