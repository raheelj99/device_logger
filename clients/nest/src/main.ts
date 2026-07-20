// Nest bootstrap: serve the REST facade. Port from PORT env, default 3001.
import 'reflect-metadata';
import { NestFactory } from '@nestjs/core';
import { AppModule } from './app.module';

async function bootstrap() {
  const app = await NestFactory.create(AppModule);
  const port = Number(process.env.PORT || 3001);
  await app.listen(port);
  console.log(`devlog-nest REST facade listening on http://localhost:${port}`);
}

bootstrap().catch((err) => {
  console.error('bootstrap failed:', err.message || err);
  process.exit(1);
});
