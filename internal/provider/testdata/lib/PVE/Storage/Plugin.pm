# Test stub of PVE::Storage::Plugin (the SectionConfig base of storage plug-ins): register and
# lookup, as the loader and the load check use them; nothing else is needed by the restic-pve
# plug-in.
package PVE::Storage::Plugin;

use strict;
use warnings;

our @registered;
my %types;

sub register {
    my ($class) = @_;
    push @registered, $class;
    $types{ $class->type() } = $class if $class->can('type');
    return;
}

sub lookup {
    my ($class, $type) = @_;
    return $types{$type} // die "unknown section type '$type'\n";
}

1;
