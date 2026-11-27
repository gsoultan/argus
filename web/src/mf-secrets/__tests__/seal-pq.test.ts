import { describe, expect, it } from 'vitest'
import { b64urlDecode, b64urlEncode, oneTimeKey, openSealed, readAAD, sealTo, stdB64Decode, stdB64Encode, writeAAD, type PQKey } from '../seal'

// Sealed by internal/secretwire.SealPQ in Go (HPKE, ML-KEM-768 + X25519), to
// the key below, as argus-secret answers a read of prod/db version 3, field
// password. The secret key is the 32-byte seed Go's hpke PrivateKey.Bytes gives.
const GO = {
  sk: 'QIpWIiUlm4P-n8MnxS1mUBmvcLwZ4NA71ZOe07Fb5d0',
  pk: 'JTt9onVHiGscUWdTuSIW5PmtHKbNn7pCA3u0VeFBcJbFplFDVXlFkLQ4k1S4JlYKKJkpFai7gJfJk0tVfpGfN2mPZGkesXdpqNC-BlSEygWRYoA5hRhkV9BIlQwQzNwophy_FeykkeCMt_i8DyJ2rSKXupTGrrgu3lMm8lJMJHpzsfM8nFoBGqQCwoMBg8gUketFB3ujjmaxExssX0N5DvCf4NqlnNY0Srh1cTZYrCWIcaAo6bFsf1HCtnDMoDO1CVWayxgH1wuum7Wg4qAePYc7Y9JFunwMx3CqmoxrhRTEULFUrAHPpwxXLiMytuZgKoSVBUwXtorBSnhK_iQtFPl_OLt7tJRsEgcXU2Ua9AMYHqJkl-E7BVQozINyeuQi1kVnlVYgZBV3jMttGlPDtAGjFvBgxde2MGIZVsGrYuPKJeOYlcPMVmMw7QrP1qB6jXwaoHDCljCvHhFWcwRWLwZsAMMoZWlH-vhf9Hh-NUsxf-xc5gswpblfQ3TOa1NgNNIbOqung9o2HWM3YlNlIZup7wFuW8V3jLp4BaOXvRC0-XVJiah-zbBv-5vPE9KFVqB0P6Mg0vJS95sgYnWpLMWw4MUkwdQ0Nox_HPHNuMhVNvdOeIA5Cjm9FDUvx7J4RtCGnkARcXDOQqJXnWDMqWGN45Yi2RFbMkwiv8aLNrpZDAt8lbGPcgt2OyK6nzeKM8q5jKXLwzgcphuBmocXH_hIQberIVG-x-uenQscc9ksdfiKU7CLCSdDstGUsRiaaqUtA9evxiREWuqSxCJfxmt-4TVjdjtO11SBQ0KA7vJAvTxjnAdkV5Oig1mwLSPFpRoLKdEyqoWR0RFOMhuCkEdYv7eEZjFwWOdrgNcGjhhgXiAa4AnGwcaGsKMcYGARjYBDRBqS9zKMuLZZEeqUhoce5pWpq7otIZdFZhhqzsCnYFbBGgmaRWldiqK7XmkVw0exjAohCxC9UMKud0xGQ8YBVrOhPzcFh6KO9BU1AQqzxktLcHRur5kGxNkxvPJfzbJosdgCJPVd7unK5dk2iQuHloxON7zG8odJQXUJ_0mIxSOuIUaJTRlt21REyJTMlDKQC_iMt9gt4SOx-VQUWZpefVkDzjMZ0fYcd6gUMaq6-kWBLFETkNXFATsrsgoFOHm_7AY2SWwFBMMxiiUykApw-EOOW8C_bZM5-hM_Y7IqRbKTrRRK3fPHthMCh_fBOCM3cHuEHCeNKXlsBoyicyxxbDXASbuY_5ockjwOptdcXaGBZ1CVRYeObbJCRiNO_pN_AoHIFbei6ZJeH_CG9Py7rwOfKBNBSeUUTpsTgGQsQzql70F0NrK-LPw6PlmXaBk2R2setsnN0GxbdNJ2wSwoRheh-LYhv9VGOlS6JtVM0HCU2bSvR8k8PCoiyzqe7gGbypJnPuiPq5aCuYVcJZbOausAMiYeH2pOgIbLFVAEgCKrcEKPapWzqJs4mjzIR6OmY1QCUspKvtmY4ig91zNoSCeaU1fIMxt8C-nFKGhJjEcCdVErgXCr99wn5eEYphfpPoqLOxPzk5z2zDYEeyPb-MfcqPRS7dxC8shdZ7R2g147tqMEnwB4mPwJGh4eiNef9UHUWVf9kLEDoTtSKQ',
  sealed: 'AnQ9euukLOw9m6il/L5Nr62leRHEM+a9rxPZMD/tr1lujcTJ5EWzgUOdZGejssF7FihTIMc18nRj1TCCbm03Na2ed1Jv4+wntX0eK0vI3gIS+1V3YPMn6PR5n2/f8+YuOlPOE2t3LGZ/g8hDvbw017rCiirD0AdCm/udYZ1tf081S+YBrZcI7QzDx4eUZfSZFiI10EnGnwS5cdtEhzbVj7CAqi59Fhtx1y8k04uhjWilRcGLgeq+T0vd6CMIayKeWCXgxphF/DDw9TE0nP+ZLcfOxOh8GyFwSX0bTvnko4KaTTMbSCLIqOe+qNps3KqXwvnAJJ/s5xErLqgJfAtr41aVpEzoA2ZnLZ/KRISYSCrXO/Tz9cwloLV5OjiL76igG/sDi05EsJnr7ycFzqQNfKuepP74yGWI33yYjJpo53q2Np0MiCeOjdHrD0jfdi6/R3UnhhtCccfJr2lT9TCw8vkyqrTI5+bJBG924v2jvlc2P//8e8yM/iuPOMRmy4PZz8sYj5qrU4mMJKG7UEOdkR1TIshDdlUL2ArSbqpkRLG3mMBqWJKRWQrtfvlqXLU+ZbQ3mer6DZu66AJNvf52Nw9GO0H7d9I3VLh84KZpLcfVxb9iofPYeK751i+ytVKdZ1AQclcF7qY/WadQ3J9AnjdfW1GK844eXa9VSHgBkSMIHwhBToyxP26SEYCsOnDuHiX+VHj+liR9MwbPt/Rvm9FsyRUhLPxZAzbJ27Z/Op/VU6rNg+5UTmYCuHceGv/mkzXGWs5KLL+S+OYizwcPccHt8r2YeW2hYZDSfrkHNnOsiO2kTsRfLZWu7ke8zVTFGzE0zbyAi0z+MIRFLzjD2+yw3pWUBk3r/RIOqABF25vMHOc5qqrDN7EJmYzNh+kd1lreIZ4jpucU5LoRieZuTz4DP3Ox5HjNBUkeFiCn2BfP4ej2hCgL13ncZsER+aQbLZgCi1qplsQNKhXnRtxC9TImfy01OIWaNdgv9aUazpOk0dkrqTE+KH5SNJhUkrFJVNUvUeyOgrtAW5uv3Vr9OrU6aKRYQMxQLiCJ8cwIFr8XCpxIayl24HWEFRF/R2don7C5jmEskdoVf0OU/VR2WuUMTjZjJG70W6lWKIBoAYbgROvyx87u70tSNdJvdHuqei83QI2l/RD7AfmNvVgOu0xGmEwiI0FV+wydk/resWwcLSoa/yXVFVcA1w836su9268kf0E5xcHraQZ6H8yESi/p9r5Vb0KQSizQKKRKAwggghbsn/Rz2l9MHBS9ooGshUBBFf6gQva5oJIyD4j9hLM830CE3KBWmSnt+HfgGdBdEVg7A9DJu+uXMMdwqnTO2Xq1aTd07Z64MxLZHTb6D8z4PVyUmxtF8S3TtpvFU6K7y3Fs+cRawtc7sZMwSC4TtbQAAuXMk3+IsZnVBh5ZrXCO05wK1MpuvVhUIDP1Wr7VdxqOkkAXxhJwF8JDuA4E2FQdg1ZYvieE1iSmAlg/xkGEuDKh63YZUAiGEdAcwOIr2ul1R5U8uw==',
}

const goKey = (): PQKey => ({ kind: 'pq', secretKey: b64urlDecode(GO.sk), publicKey: b64urlDecode(GO.pk) })

describe('post-quantum sealing, byte for byte with secretwire', () => {
  it('opens what argus-secret sealed', async () => {
    const plain = await openSealed(goKey(), stdB64Decode(GO.sealed), readAAD('prod/db', 3, 'password'))
    expect(new TextDecoder().decode(plain)).toBe('hunter2')
  })

  it('refuses it as another secret, version or field', async () => {
    for (const aad of [readAAD('prod/other', 3, 'password'), readAAD('prod/db', 2, 'password'), readAAD('prod/db', 3, '')]) {
      await expect(openSealed(goKey(), stdB64Decode(GO.sealed), aad)).rejects.toThrow(/cannot be opened/)
    }
  })

  it('refuses it with another key, changed, or as a v1 answer', async () => {
    await expect(openSealed(await oneTimeKey(), stdB64Decode(GO.sealed), readAAD('prod/db', 3, 'password'))).rejects.toThrow()
    for (const i of [1, 1120, 1121, stdB64Decode(GO.sealed).length - 1]) {
      const bent = stdB64Decode(GO.sealed)
      bent[i]! ^= 1
      await expect(openSealed(goKey(), bent, readAAD('prod/db', 3, 'password'))).rejects.toThrow()
    }
    const v1 = stdB64Decode(GO.sealed)
    v1[0] = 1
    await expect(openSealed(goKey(), v1, readAAD('prod/db', 3, 'password'))).rejects.toThrow()
  })

  it('a one-time key is post-quantum, and seals to a post-quantum key in format v2', async () => {
    const k = await oneTimeKey()
    expect(k.kind).toBe('pq')
    expect(k.publicKey.length).toBe(1184 + 32)
    const sealed = await sealTo(k.publicKey, new TextEncoder().encode('new value'), writeAAD('prod/db'))
    expect(sealed[0]).toBe(2)
    expect(sealed.length).toBe(1 + 1120 + 'new value'.length + 16)
    expect(new TextDecoder().decode(await openSealed(k, sealed, writeAAD('prod/db')))).toBe('new value')
    await expect(openSealed(k, sealed, writeAAD('prod/other'))).rejects.toThrow()
    expect(stdB64Decode(stdB64Encode(sealed))).toEqual(sealed)
  })

  it('makes a value for Go to open (TestBrowserSealOpensInGo)', async () => {
    const sealed = await sealTo(b64urlDecode(GO.pk), new TextEncoder().encode('from the console'), writeAAD('prod/db'))
    expect(b64urlEncode(sealed).length).toBeGreaterThan(1500)
    if (process.env.PRINT_VECTOR) console.log('BROWSER_SEALED=' + stdB64Encode(sealed))
  })
})
