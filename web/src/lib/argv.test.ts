import { test } from 'node:test';
import assert from 'node:assert/strict';
import { parseArgv, argvPreview } from './argv.ts';

test('parseArgv trims, drops empties, keeps quotes literally', () => {
  assert.deepEqual(parseArgv('uname\n  -a  \n\n'), ['uname', '-a']);
  assert.deepEqual(parseArgv('echo\n"a;id"\n\'x\'\r\n'), ['echo', '"a;id"', "'x'"]);
  assert.deepEqual(parseArgv(''), []);
  assert.deepEqual(parseArgv('\n \n'), []);
});

test('argvPreview quotes only what the shell would mangle', () => {
  assert.equal(argvPreview(['uname', '-a']), 'uname -a');
  assert.equal(argvPreview(['echo', 'a;id']), "echo 'a;id'");
  assert.equal(argvPreview(['echo', "it's"]), "echo 'it'\\''s'");
  assert.equal(argvPreview(['touch', 'two words']), "touch 'two words'");
  assert.equal(argvPreview([]), '');
});
