# Test stub of PVE::Storage: only the API version constants the plug-in and the loader use.
# STUB_APIVER sets the version a test simulates (Proxmox VE 9.2 at HEAD: 17, age 8).
package PVE::Storage;

use strict;
use warnings;

use constant APIVER => $ENV{STUB_APIVER} // 17;
use constant APIAGE => 8;

our $SAFE_CHAR_CLASS_RE = qr/[a-zA-Z0-9\-\.\+\=\_]/;

1;
