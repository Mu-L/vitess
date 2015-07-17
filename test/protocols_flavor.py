#!/usr/bin/env python

import logging

class ProtocolsFlavor(object):
  """Base class for protocols"""

  def binlog_player_protocol(self):
    """Returns the name of the binlog player protocol to use."""
    raise NotImplementedError('Not implemented in the base class')

  def vtctl_client_protocol(self):
    """Returns the protocol to use for vtctl connections.
    This is just for the go client."""
    raise NotImplementedError('Not implemented in the base class')

  def vtctl_python_client_protocol(self):
    """Returns the protocol to use for vtctl connections.
    This is just for the python client."""
    raise NotImplementedError('Not implemented in the base class')

  def vtworker_client_protocol(self):
    """Returns the protocol to use for vtworker connections."""
    raise NotImplementedError('Not implemented in the base class')

  def tablet_manager_protocol(self):
    """Returns the protocol to use for the tablet manager protocol."""
    raise NotImplementedError('Not implemented in the base class')

  def tabletconn_protocol(self):
    """Returns the protocol to use for connections from vtctl/vtgate to
    vttablet."""
    raise NotImplementedError('Not implemented in the base class')

  def vtgate_protocol_flags(self):
    """Returns the flags to use for specifying the vtgate protocol."""
    raise NotImplementedError('Not implemented in the base class')

  def rpc_timeout_message(self):
    """Returns the error message used by the protocol to indicate a timeout."""
    raise NotImplementedError('Not implemented in the base class')

  def service_map(self):
    """Returns a list of entries for the service map to enable all
    relevant protocols in all servers."""
    raise NotImplementedError('Not implemented in the base class')


__knows_protocols_flavor_map = {}
__protocols_flavor = None

def protocols_flavor():
  return __protocols_flavor

def set_protocols_flavor(flavor):
  global __protocols_flavor

  if not flavor:
    flavor = 'gorpc'

  klass = __knows_protocols_flavor_map.get(flavor, None)
  if not klass:
    logging.error('Unknown protocols flavor %s', flavor)
    exit(1)
  __protocols_flavor = klass()

  logging.debug('Using protocols flavor %s', flavor)
