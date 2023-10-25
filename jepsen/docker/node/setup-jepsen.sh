#!/bin/bash

# We add our hostname to the shared volume, so that control can find us
echo "Adding hostname to shared volume" >> /var/log/jepsen-setup.log
echo `hostname` >> /var/jepsen/shared/nodes
