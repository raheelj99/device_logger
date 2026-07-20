// Jest + ts-jest. Unit tests are infra-free: they exercise the pure builders
// and the protobuf codec, never a live devlogd/Redis/MinIO.
module.exports = {
  preset: 'ts-jest',
  testEnvironment: 'node',
  testMatch: ['**/*.spec.ts'],
  rootDir: 'src',
};
