import express from 'express';
import { submitOrder } from './orders';

const app = express();
app.use(express.json());

app.post('/checkout', checkoutHandler);

// checkoutHandler validates the cart and submits the order to the gateway.
async function checkoutHandler(req: any, res: any) {
  const userId = req.body.user_id;
  const amount = req.body.amount;
  const result = await submitOrder(userId, amount);
  res.json(result);
}

app.listen(3000);
