# Test stub of PVE::Storage::Plugin (the SectionConfig base of storage plug-ins): register is
# what the loader calls; nothing else is needed by the restic-pve plug-in.
package PVE::Storage::Plugin;

use strict;
use warnings;

our @registered;

sub register { my ($class) = @_; push @registered, $class; return }

1;
