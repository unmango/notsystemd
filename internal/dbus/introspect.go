package dbus

import (
	"fmt"
	"strings"
)

// Clients build their proxies from these documents, so every member they use
// has to be listed: a missing method reads as undefined in dbus-next, and a
// missing interface throws.

const doctype = `<!DOCTYPE node PUBLIC "-//freedesktop//DTD D-BUS Object Introspection 1.0//EN"
 "http://www.freedesktop.org/standards/dbus/1.0/introspect.dtd">
`

const standardXML = ` <interface name="org.freedesktop.DBus.Peer">
  <method name="Ping"/>
 </interface>
 <interface name="org.freedesktop.DBus.Introspectable">
  <method name="Introspect">
   <arg name="xml" type="s" direction="out"/>
  </method>
 </interface>
 <interface name="org.freedesktop.DBus.Properties">
  <method name="Get">
   <arg name="interface_name" type="s" direction="in"/>
   <arg name="property_name" type="s" direction="in"/>
   <arg name="value" type="v" direction="out"/>
  </method>
  <method name="GetAll">
   <arg name="interface_name" type="s" direction="in"/>
   <arg name="props" type="a{sv}" direction="out"/>
  </method>
  <method name="Set">
   <arg name="interface_name" type="s" direction="in"/>
   <arg name="property_name" type="s" direction="in"/>
   <arg name="value" type="v" direction="in"/>
  </method>
  <signal name="PropertiesChanged">
   <arg type="s" name="interface_name"/>
   <arg type="a{sv}" name="changed_properties"/>
   <arg type="as" name="invalidated_properties"/>
  </signal>
 </interface>
`

const managerXML = doctype + `<node>
` + standardXML + ` <interface name="org.freedesktop.systemd1.Manager">
  <property name="Version" type="s" access="read"/>
  <property name="SystemState" type="s" access="read"/>
  <method name="GetUnit">
   <arg type="s" name="name" direction="in"/>
   <arg type="o" name="unit" direction="out"/>
  </method>
  <method name="LoadUnit">
   <arg type="s" name="name" direction="in"/>
   <arg type="o" name="unit" direction="out"/>
  </method>
  <method name="Subscribe"/>
  <method name="Unsubscribe"/>
  <signal name="UnitFilesChanged"/>
 </interface>
 <node name="unit"/>
</node>
`

const unitXML = doctype + `<node>
` + standardXML + ` <interface name="org.freedesktop.systemd1.Unit">
  <property name="Id" type="s" access="read"/>
  <property name="Names" type="as" access="read"/>
  <property name="Description" type="s" access="read"/>
  <property name="LoadState" type="s" access="read"/>
  <property name="ActiveState" type="s" access="read"/>
  <property name="SubState" type="s" access="read"/>
 </interface>
 <interface name="org.freedesktop.systemd1.Service">
  <property name="StatusText" type="s" access="read"/>
  <property name="MainPID" type="u" access="read"/>
  <property name="Result" type="s" access="read"/>
  <property name="ExecMainStatus" type="i" access="read"/>
 </interface>
</node>
`

const busXML = doctype + `<node>
 <interface name="org.freedesktop.DBus">
  <method name="Hello">
   <arg direction="out" type="s"/>
  </method>
  <method name="AddMatch">
   <arg direction="in" type="s"/>
  </method>
  <method name="RemoveMatch">
   <arg direction="in" type="s"/>
  </method>
  <method name="GetNameOwner">
   <arg direction="in" type="s"/>
   <arg direction="out" type="s"/>
  </method>
  <method name="NameHasOwner">
   <arg direction="in" type="s"/>
   <arg direction="out" type="b"/>
  </method>
  <method name="ListNames">
   <arg direction="out" type="as"/>
  </method>
  <method name="GetId">
   <arg direction="out" type="s"/>
  </method>
  <signal name="NameOwnerChanged">
   <arg type="s"/>
   <arg type="s"/>
   <arg type="s"/>
  </signal>
 </interface>
 <interface name="org.freedesktop.DBus.Introspectable">
  <method name="Introspect">
   <arg direction="out" type="s"/>
  </method>
 </interface>
 <interface name="org.freedesktop.DBus.Peer">
  <method name="Ping"/>
 </interface>
</node>
`

// introspect describes the object at path: the manager, a unit, or one of
// the nodes above them.
func (s *Server) introspect(m *Message, path string) *Message {
	switch path {
	case managerPath:
		return returnOf(m, "s", managerXML)
	case "/", "/org", "/org/freedesktop":
		child := strings.TrimPrefix(strings.TrimPrefix(managerPath, path), "/")
		child, _, _ = strings.Cut(child, "/")
		return returnOf(m, "s", fmt.Sprintf("%s<node>\n <node name=%q/>\n</node>\n", doctype, child))
	case strings.TrimSuffix(unitPrefix, "/"):
		return returnOf(m, "s", doctype+"<node>\n</node>\n")
	}
	if _, ok := unitName(path); ok {
		return returnOf(m, "s", unitXML)
	}
	return errorReply(m, "org.freedesktop.DBus.Error.UnknownObject",
		fmt.Sprintf("Unknown object '%s'.", path))
}
