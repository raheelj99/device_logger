// Wires the two plane services to the REST facade. Exported so a bootstrap
// context (e2e.ts) can resolve the services without HTTP.
import { Module } from '@nestjs/common';
import { DevlogController } from './devlog.controller';
import { PublisherService } from './publisher.service';
import { ObserverService } from './observer.service';

@Module({
  controllers: [DevlogController],
  providers: [PublisherService, ObserverService],
  exports: [PublisherService, ObserverService],
})
export class DevlogModule {}
