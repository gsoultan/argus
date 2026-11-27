// `@mantine/core`, in a section's bundle: the console's copy, so a section
// draws with the console's theme and components. A name a section imports
// must be listed here; the section's build fails on one that is not.
import { shell } from './shell'

export const {
  Alert, Anchor, Badge, Box, Button, Checkbox, Code, CopyButton, Group, Modal, NumberInput, SegmentedControl, Select, Stack, Switch, Table, Tabs, Text, TextInput, Textarea,
} = shell.mantine
