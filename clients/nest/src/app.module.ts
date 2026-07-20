import { Module } from '@nestjs/common';
import { DevlogModule } from './devlog/devlog.module';

@Module({
  imports: [DevlogModule],
})
export class AppModule {}
