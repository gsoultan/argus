import type { Asset, AssetKind } from '~/types/domain'

/**
 * The three types of asset, in the words the console uses for them.
 *
 * Stored as `assets.kind`: `server`, `workstation` and `android`. A device is
 * `android` in the data because that is how it is reached (the Argus app on
 * it); people call it a device, so the console does. A workstation is stored
 * and opened exactly as a server is; the type says a person uses it.
 */
export type AssetType = 'server' | 'workstation' | 'device'

export function assetTypeOf(a: { kind?: AssetKind } | undefined): AssetType {
  if (a?.kind === 'android') return 'device'
  if (a?.kind === 'workstation') return 'workstation'
  return 'server'
}

export const ASSET_TYPES: readonly AssetType[] = ['server', 'workstation', 'device']

export const ASSET_TYPE_LABEL: Record<AssetType, { one: string; many: string; lower: string }> = {
  server: { one: 'Server', many: 'Servers', lower: 'server' },
  workstation: { one: 'Workstation', many: 'Workstations', lower: 'workstation' },
  device: { one: 'Device', many: 'Devices', lower: 'device' },
}

/** One line on what each type is, for the places an admin picks one. */
export const ASSET_TYPE_BLURB: Record<AssetType, string> = {
  server: 'A machine nobody sits at. People connect to do work on it.',
  workstation: 'A laptop or PC that belongs to someone. You connect to help them or fix it.',
  device: 'A phone or tablet with the Argus app on it.',
}

/** The noun for one asset, lower case: "server", "workstation", "device". */
export function assetNoun(a: Asset | undefined): string {
  return ASSET_TYPE_LABEL[assetTypeOf(a)].lower
}
