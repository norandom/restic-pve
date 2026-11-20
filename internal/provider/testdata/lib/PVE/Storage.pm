# Test stub of PVE::Storage: the API version constants, and the loading of custom plug-ins as
# pve-storage src/PVE/Storage.pm does it (each Custom/*.pm: require, isa, api window, register;
# a failure is a warning). STUB_APIVER sets the version a test simulates (Proxmox VE 9.2 at
# HEAD: 17, age 8). The real loader reads only /usr/share/perl5/PVE/Storage/Custom; the stub
# takes the first PVE/Storage/Custom in @INC, so a test can put the plug-in below a temp root.
package PVE::Storage;

use strict;
use warnings;

use PVE::Storage::Plugin;

use constant APIVER => $ENV{STUB_APIVER} // 17;
use constant APIAGE => 8;

our $SAFE_CHAR_CLASS_RE = qr/[a-zA-Z0-9\-\.\+\=\_]/;

for my $inc (@INC) {
    next if ref $inc;
    my $dir = "$inc/PVE/Storage/Custom";
    next if !-d $dir;
    opendir(my $dh, $dir) or last;
    for my $file (sort grep { m/^(.*\.pm)$/ } readdir($dh)) {
        my $modname = 'PVE::Storage::Custom::' . $file;
        $modname =~ s!\.pm$!!;
        eval {
            require "PVE/Storage/Custom/$file";
            die "not derived from PVE::Storage::Plugin\n" if !$modname->isa('PVE::Storage::Plugin');
            die "does not provide an api() method\n" if !$modname->can('api');
            my $version = $modname->api();
            die "implements an API version newer than current ($version > " . APIVER . ")\n" if $version > APIVER;
            my $min_version = (APIVER - APIAGE);
            die "API version too old, please update the plugin ($version < $min_version)\n" if $version < $min_version;
            $modname->register();
            warn "Plugin \"$modname\" is implementing an older storage API, an upgrade is recommended\n" if $version != APIVER;
        };
        warn "Error loading storage plugin \"$modname\": $@" if $@;
    }
    closedir($dh);
    last;
}

1;
