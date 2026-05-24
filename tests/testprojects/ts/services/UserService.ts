import { User } from '../models/User';
export namespace Data {
    export class UserStore {
        getUser(id: string): User {
            return { id, name: 'Test' };
        }
    }
}
