ObjC.import('Foundation');

function fail(message) {
  throw new Error(message);
}

function isKind(value, objectiveCClass) {
  return Boolean(value) && Boolean(value.isKindOfClass(objectiveCClass));
}

function dictionaryKeys(dictionary) {
  const keys = dictionary.allKeys;
  const result = [];
  for (let index = 0; index < Number(keys.count); index++) {
    const key = keys.objectAtIndex(index);
    if (!isKind(key, $.NSString)) fail('plist dictionary key must be a string');
    result.push(ObjC.unwrap(key));
  }
  return result;
}

function dictionaryValue(dictionary, key) {
  return dictionary.objectForKey($(key));
}

function integerValue(value) {
  if (!isKind(value, $.NSNumber)) return null;
  const unwrapped = ObjC.unwrap(value);
  if (typeof unwrapped !== 'number' || !Number.isInteger(unwrapped)) return null;
  return unwrapped;
}

function positiveSettingIsExportable(setting) {
  const allowed = new Set([
    'kSecTrustSettingsPolicy',
    'kSecTrustSettingsPolicyName',
    'kSecTrustSettingsKeyUsage',
    'kSecTrustSettingsResult'
  ]);
  for (const key of dictionaryKeys(setting)) {
    if (!allowed.has(key)) return false;
  }
  const policy = dictionaryValue(setting, 'kSecTrustSettingsPolicy');
  const policyName = dictionaryValue(setting, 'kSecTrustSettingsPolicyName');
  if (policy) {
    if (!isKind(policy, $.NSData) || !isKind(policyName, $.NSString) || ObjC.unwrap(policyName) !== 'sslServer') return false;
  } else if (policyName) {
    return false;
  }
  const usageValue = dictionaryValue(setting, 'kSecTrustSettingsKeyUsage');
  if (usageValue) {
    const usage = integerValue(usageValue);
    if (usage === null || ![-1, 0xffffffff, 0x1, 0x8].includes(usage)) return false;
  }
  return true;
}

function run(argv) {
  if (argv.length !== 1) fail('usage: macos-trust-settings.js <export.plist>');
  const data = $.NSData.dataWithContentsOfFile($(argv[0]));
  if (!data) fail('cannot read exported trust settings plist');
  const document = $.NSPropertyListSerialization.propertyListWithDataOptionsFormatError(data, 0, null, null);
  if (!document || !isKind(document, $.NSDictionary)) fail('cannot parse exported trust settings plist');

  const trustVersion = integerValue(dictionaryValue(document, 'trustVersion'));
  const trustList = dictionaryValue(document, 'trustList');
  if (trustVersion !== 1 || !isKind(trustList, $.NSDictionary)) fail('unsupported macOS trust settings document');

  const lines = [];
  for (const rawFingerprint of dictionaryKeys(trustList)) {
    const fingerprint = rawFingerprint.trim().toUpperCase();
    if (!/^[0-9A-F]{40}$/.test(fingerprint)) fail('unsupported non-certificate/default trust entry: ' + rawFingerprint);
    const entry = dictionaryValue(trustList, rawFingerprint);
    if (!isKind(entry, $.NSDictionary)) fail('trust entry must be a dictionary for ' + fingerprint);
    const settings = dictionaryValue(entry, 'trustSettings');
    if (!settings) {
      lines.push('allow ' + fingerprint);
      continue;
    }
    if (!isKind(settings, $.NSArray)) fail('trustSettings must be an array for ' + fingerprint);

    let allow = Number(settings.count) === 0;
    let deny = false;
    for (let index = 0; index < Number(settings.count); index++) {
      const setting = settings.objectAtIndex(index);
      if (!isKind(setting, $.NSDictionary)) fail('trust setting must be an object for ' + fingerprint);
      const resultValue = dictionaryValue(setting, 'kSecTrustSettingsResult');
      const result = resultValue ? integerValue(resultValue) : 1;
      if (result === null || ![0, 1, 2, 3, 4].includes(result)) fail('invalid trust result for ' + fingerprint);
      if (result === 3) deny = true;
      if ((result === 1 || result === 2) && positiveSettingIsExportable(setting)) allow = true;
    }
    if (deny) lines.push('deny ' + fingerprint);
    else if (allow) lines.push('allow ' + fingerprint);
  }
  return lines.join('\n');
}
